// Package anomaly provides intelligent rate and usage anomaly detection for the AI Gateway.
// It monitors per-user, per-API-key, and per-department Token consumption rates (TPM) and
// request frequencies (QPS) using sliding windows, detects abnormal behaviors such as
// infinite loops, data scraping, and API key leakage, and triggers disposition actions
// including throttling, freezing, and alerting.
package anomaly

import (
	"sync"
	"time"
)

// DispositionType defines the action taken when an anomaly is detected.
type DispositionType string

const (
	DispositionThrottle     DispositionType = "throttle"      // temporarily rate-limit the key
	DispositionFreeze       DispositionType = "freeze"        // temporarily freeze the API key
	DispositionAlert        DispositionType = "alert"         // send enterprise alert
	DispositionThrottleThen DispositionType = "throttle_then" // throttle then escalate if sustained
)

// DispositionStrategy describes what action to take for a given anomaly type.
type DispositionStrategy struct {
	Action         DispositionType `json:"action"`
	ThrottleBurst  int             `json:"throttle_burst,omitempty"`  // burst limit when throttling
	ThrottleRate   float64         `json:"throttle_rate,omitempty"`   // sustained rate when throttling
	ThrottleWindow time.Duration   `json:"throttle_window,omitempty"` // how long to throttle
	FreezeDuration time.Duration   `json:"freeze_duration,omitempty"` // freeze duration
	CooldownAfter  time.Duration   `json:"cooldown_after,omitempty"`  // minimum interval between actions
}

// AnomalyType classifies the kind of detected anomaly.
type AnomalyType string

const (
	AnomalyRateSpike        AnomalyType = "rate_spike"        // TPM/QPS exceeded threshold
	AnomalyInfiniteLoop     AnomalyType = "infinite_loop"     // repeated identical prompt pattern
	AnomalyDataScraping     AnomalyType = "data_scraping"     // systematic data extraction
	AnomalyConcurrencyAbuse AnomalyType = "concurrency_abuse" // unauthorized concurrent pressure
	AnomalyKeyLeak          AnomalyType = "key_leak"          // API key leakage / stolen key
	AnomalySustainedHigh    AnomalyType = "sustained_high"    // prolonged above-threshold usage
)

// Severity represents the severity level of an anomaly event.
type Severity string

const (
	SeverityLow      Severity = "low"
	SeverityMedium   Severity = "medium"
	SeverityHigh     Severity = "high"
	SeverityCritical Severity = "critical"
)

// Event records a single anomaly detection event.
type Event struct {
	ID             string          `json:"id"`
	AnomalyType    AnomalyType     `json:"anomaly_type"`
	Severity       Severity        `json:"severity"`
	Principal      string          `json:"principal"` // API key or user ID
	Provider       string          `json:"provider"`  // auth provider
	Department     string          `json:"department,omitempty"`
	ClientIP       string          `json:"client_ip,omitempty"`
	Model          string          `json:"model,omitempty"` // affected model
	Disposition    DispositionType `json:"disposition"`
	ObservedValue  float64         `json:"observed_value"` // observed TPM or QPS
	BaselineValue  float64         `json:"baseline_value"` // historical baseline
	Multiplier     float64         `json:"multiplier"`     // observed / baseline
	TokensConsumed int64           `json:"tokens_consumed"`
	Message        string          `json:"message"`
	TriggeredAt    time.Time       `json:"triggered_at"`
	ResolvedAt     *time.Time      `json:"resolved_at,omitempty"`
}

// SlidingWindow stores per-key token and request counts over a time window.
// Each Record appends a new bucket; expired buckets are evicted on the next Record.
type SlidingWindow struct {
	mu            sync.RWMutex
	windowSize    time.Duration
	buckets       []windowBucket
	totalTokens   int64
	totalRequests int64
}

type windowBucket struct {
	startTime time.Time
	tokens    int64
	requests  int64
}

// NewSlidingWindow creates a sliding window with the given duration and number of buckets.
func NewSlidingWindow(windowSize time.Duration, numBuckets int) *SlidingWindow {
	if numBuckets < 1 {
		numBuckets = 10
	}
	return &SlidingWindow{
		windowSize: windowSize,
		buckets:    make([]windowBucket, 0, numBuckets),
	}
}

// Record adds a token count and request count to the sliding window at the current time.
func (w *SlidingWindow) Record(now time.Time, tokens int64, requests int64) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	w.evict(now)
	w.totalTokens += tokens
	w.totalRequests += requests
	w.buckets = append(w.buckets, windowBucket{startTime: now, tokens: tokens, requests: requests})
}

// TPM returns the tokens-per-minute rate over the current window.
func (w *SlidingWindow) TPM(now time.Time) float64 {
	if w == nil {
		return 0
	}
	w.mu.RLock()
	total := w.totalTokens
	w.mu.RUnlock()

	elapsed := w.windowSize
	if elapsed <= 0 {
		return 0
	}
	return float64(total) / elapsed.Minutes()
}

// QPS returns the queries-per-second rate over the current window.
func (w *SlidingWindow) QPS(now time.Time) float64 {
	if w == nil {
		return 0
	}
	w.mu.RLock()
	total := w.totalRequests
	w.mu.RUnlock()

	elapsed := w.windowSize
	if elapsed <= 0 {
		return 0
	}
	return float64(total) / elapsed.Seconds()
}

// Reset clears all counters in the window.
func (w *SlidingWindow) Reset() {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	w.buckets = w.buckets[:0]
	w.totalTokens = 0
	w.totalRequests = 0
}

// Snapshot returns a copy of the current window totals.
func (w *SlidingWindow) Snapshot() (tokens int64, requests int64) {
	if w == nil {
		return 0, 0
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.totalTokens, w.totalRequests
}

// evict removes expired buckets. Must be called with w.mu held.
func (w *SlidingWindow) evict(now time.Time) {
	cutoff := now.Add(-w.windowSize)
	keep := 0
	for _, b := range w.buckets {
		if b.startTime.IsZero() || b.startTime.After(cutoff) {
			break
		}
		w.totalTokens -= b.tokens
		w.totalRequests -= b.requests
		keep++
	}
	if keep > 0 {
		n := copy(w.buckets, w.buckets[keep:])
		w.buckets = w.buckets[:n]
	}
}

// Config defines the global anomaly detection configuration.
type Config struct {
	// Enabled toggles the anomaly detection engine.
	Enabled bool `yaml:"enabled" json:"enabled"`

	// WindowSize is the sliding window duration for rate calculation.
	WindowSize time.Duration `yaml:"window-size" json:"window-size"`

	// NumBuckets is the number of sub-buckets in the sliding window.
	NumBuckets int `yaml:"num-buckets" json:"num-buckets"`

	// RateAnomalyThreshold is the multiplier over baseline to trigger a rate anomaly.
	// E.g., 3.0 means TPM > 300% of baseline triggers.
	RateAnomalyThreshold float64 `yaml:"rate-anomaly-threshold" json:"rate-anomaly-threshold"`

	// AbsoluteTPMLimit is an absolute TPM limit that triggers immediately regardless of baseline.
	AbsoluteTPMLimit int64 `yaml:"absolute-tpm-limit" json:"absolute-tpm-limit"`

	// AbsoluteQPSLimit is an absolute QPS limit that triggers immediately regardless of baseline.
	AbsoluteQPSLimit float64 `yaml:"absolute-qps-limit" json:"absolute-qps-limit"`

	// InfiniteLoopMinHits is the minimum number of identical requests to flag as an infinite loop.
	InfiniteLoopMinHits int `yaml:"infinite-loop-min-hits" json:"infinite-loop-min-hits"`

	// InfiniteLoopWindow is the time window within which identical requests are counted.
	InfiniteLoopWindow time.Duration `yaml:"infinite-loop-window" json:"infinite-loop-window"`

	// ConcurrencyAbuseThreshold is the number of concurrent requests from a single key to flag.
	ConcurrencyAbuseThreshold int `yaml:"concurrency-abuse-threshold" json:"concurrency-abuse-threshold"`

	// SustainedHighWindow is the duration of sustained above-threshold usage to escalate.
	SustainedHighWindow time.Duration `yaml:"sustained-high-window" json:"sustained-high-window"`

	// SustainedHighThreshold is the multiplier for sustained high detection.
	SustainedHighThreshold float64 `yaml:"sustained-high-threshold" json:"sustained-high-threshold"`

	// CooldownInterval is the minimum time between consecutive disposition actions for the same key.
	CooldownInterval time.Duration `yaml:"cooldown-interval" json:"cooldown-interval"`

	// MaxEventsPerKey caps the in-memory event ledger size per principal.
	MaxEventsPerKey int `yaml:"max-events-per-key" json:"max-events-per-key"`

	// Strategies maps anomaly types to disposition strategies.
	Strategies map[AnomalyType]DispositionStrategy `yaml:"strategies" json:"strategies"`
}

// DefaultConfig returns a sane default configuration.
func DefaultConfig() Config {
	return Config{
		WindowSize:                5 * time.Minute,
		NumBuckets:                10,
		RateAnomalyThreshold:      3.0,
		AbsoluteTPMLimit:          0,
		AbsoluteQPSLimit:          0,
		InfiniteLoopMinHits:       20,
		InfiniteLoopWindow:        1 * time.Minute,
		ConcurrencyAbuseThreshold: 50,
		SustainedHighWindow:       15 * time.Minute,
		SustainedHighThreshold:    2.0,
		CooldownInterval:          5 * time.Minute,
		MaxEventsPerKey:           100,
		Strategies: map[AnomalyType]DispositionStrategy{
			AnomalyRateSpike: {
				Action:         DispositionThrottleThen,
				ThrottleBurst:  100,
				ThrottleRate:   20,
				ThrottleWindow: 5 * time.Minute,
				CooldownAfter:  5 * time.Minute,
			},
			AnomalyInfiniteLoop: {
				Action:         DispositionFreeze,
				FreezeDuration: 15 * time.Minute,
				CooldownAfter:  10 * time.Minute,
			},
			AnomalyDataScraping: {
				Action:         DispositionFreeze,
				FreezeDuration: 30 * time.Minute,
				CooldownAfter:  15 * time.Minute,
			},
			AnomalyConcurrencyAbuse: {
				Action:         DispositionThrottle,
				ThrottleBurst:  50,
				ThrottleRate:   10,
				ThrottleWindow: 10 * time.Minute,
				CooldownAfter:  5 * time.Minute,
			},
			AnomalyKeyLeak: {
				Action:         DispositionFreeze,
				FreezeDuration: 60 * time.Minute,
				CooldownAfter:  30 * time.Minute,
			},
			AnomalySustainedHigh: {
				Action:        DispositionAlert,
				CooldownAfter: 15 * time.Minute,
			},
		},
	}
}
