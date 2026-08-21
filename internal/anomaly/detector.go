package anomaly

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// Engine is the anomaly detection engine.
// It monitors per-principal sliding windows, detects rate anomalies, and
// triggers disposition actions.
type Engine struct {
	cfg        Config
	mu         sync.RWMutex
	windows    map[string]*SlidingWindow   // keyed by principal|provider
	baselines  map[string]*baseline        // historical baseline per principal
	loopCounts map[string]*loopCounter     // repeating prompt detection
	concurrent map[string]*concurrentTrack // in-flight requests
	events     map[string][]*Event         // event ledger, keyed by principal
	cooldowns  map[string]time.Time        // last disposition time per principal
	alertFn    AlertFunc
	nextID     int64
}

// AlertFunc is called when a disposition action is triggered.
type AlertFunc func(ctx context.Context, event *Event)

type baseline struct {
	avgTPM float64
	avgQPS float64
	count  int
}

type loopCounter struct {
	mu      sync.Mutex
	recent  map[string]*loopEntry
	expires time.Duration
}

type loopEntry struct {
	firstSeen time.Time
	count     int
}

type concurrentTrack struct {
	mu       sync.Mutex
	inflight map[string]int // key -> count
}

// NewEngine creates a new anomaly detection engine with the given config.
func NewEngine(cfg Config) *Engine {
	return &Engine{
		cfg:        cfg,
		windows:    make(map[string]*SlidingWindow),
		baselines:  make(map[string]*baseline),
		loopCounts: make(map[string]*loopCounter),
		concurrent: make(map[string]*concurrentTrack),
		events:     make(map[string][]*Event),
		cooldowns:  make(map[string]time.Time),
		nextID:     1,
	}
}

// SetAlertFunc registers a callback for triggered disposition actions.
func (e *Engine) SetAlertFunc(fn AlertFunc) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.alertFn = fn
}

// RecordRequest records a token consumption and request event for the given principal.
func (e *Engine) RecordRequest(ctx context.Context, principal, provider, model, clientIP, department string, tokens int64, now time.Time) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	key := e.windowKey(principal, provider)
	sw := e.windows[key]
	if sw == nil {
		sw = NewSlidingWindow(e.cfg.WindowSize, e.cfg.NumBuckets)
		e.windows[key] = sw
	}
	sw.Record(now, tokens, 1)

	// Update baseline
	bl := e.baselines[key]
	if bl == nil {
		e.baselines[key] = &baseline{avgTPM: float64(tokens), avgQPS: 1, count: 1}
	} else {
		tpm := sw.TPM(now)
		qps := sw.QPS(now)
		bl.avgTPM = (bl.avgTPM*float64(bl.count) + tpm) / float64(bl.count+1)
		bl.avgQPS = (bl.avgQPS*float64(bl.count) + qps) / float64(bl.count+1)
		bl.count++
	}

	// Run detection
	e.detect(ctx, principal, provider, model, clientIP, department, tokens, now)
}

// detect runs all detection checks and triggers dispositions.
func (e *Engine) detect(ctx context.Context, principal, provider, model, clientIP, department string, tokens int64, now time.Time) {
	if !e.cfg.Enabled {
		return
	}
	key := e.windowKey(principal, provider)
	sw := e.windows[key]
	bl := e.baselines[key]
	if sw == nil {
		return
	}

	tpm := sw.TPM(now)
	qps := sw.QPS(now)

	blTPM := 0.0
	blQPS := 0.0
	if bl != nil {
		blTPM = bl.avgTPM
		blQPS = bl.avgQPS
	}

	// 1. Absolute limit check
	if e.cfg.AbsoluteTPMLimit > 0 && int64(tpm) > e.cfg.AbsoluteTPMLimit {
		e.trigger(ctx, principal, provider, model, clientIP, department, AnomalyRateSpike, SeverityCritical, tpm, blTPM, tokens, now, "")
		return
	}
	if e.cfg.AbsoluteQPSLimit > 0 && qps > e.cfg.AbsoluteQPSLimit {
		e.trigger(ctx, principal, provider, model, clientIP, department, AnomalyRateSpike, SeverityCritical, qps, blQPS, tokens, now, "")
		return
	}

	// 2. Rate spike (multiplier over baseline)
	if bl != nil && bl.count >= 5 {
		if blTPM > 0 && tpm/blTPM > e.cfg.RateAnomalyThreshold {
			e.trigger(ctx, principal, provider, model, clientIP, department, AnomalyRateSpike, SeverityHigh, tpm, blTPM, tokens, now, "")
		}
		if blQPS > 0 && qps/blQPS > e.cfg.RateAnomalyThreshold {
			e.trigger(ctx, principal, provider, model, clientIP, department, AnomalyRateSpike, SeverityHigh, qps, blQPS, tokens, now, "")
		}
	}

	// 3. Sustained high detection
	if bl != nil && bl.count >= 10 && blTPM > 0 {
		if tpm/blTPM > e.cfg.SustainedHighThreshold {
			e.trigger(ctx, principal, provider, model, clientIP, department, AnomalySustainedHigh, SeverityMedium, tpm, blTPM, tokens, now, "sustained high usage detected")
		}
	}
}

// RecordInfiniteLoopCheck records a request for infinite loop detection.
// Returns true if the request is part of an infinite loop pattern.
func (e *Engine) RecordInfiniteLoopCheck(principal, provider, requestHash string, now time.Time) bool {
	if e == nil || !e.cfg.Enabled {
		return false
	}
	e.mu.Lock()
	lc, ok := e.loopCounts[principal]
	if !ok {
		lc = &loopCounter{
			recent:  make(map[string]*loopEntry),
			expires: e.cfg.InfiniteLoopWindow,
		}
		e.loopCounts[principal] = lc
	}
	e.mu.Unlock()

	lc.mu.Lock()
	defer lc.mu.Unlock()

	entry, exists := lc.recent[requestHash]
	if !exists {
		lc.recent[requestHash] = &loopEntry{firstSeen: now, count: 1}
		return false
	}
	if now.Sub(entry.firstSeen) > lc.expires {
		entry.firstSeen = now
		entry.count = 1
		return false
	}
	entry.count++
	if entry.count >= e.cfg.InfiniteLoopMinHits {
		return true
	}
	return false
}

// AcquireConcurrencySlot tracks a concurrent request slot.
// Returns the in-flight count for the principal after acquisition.
func (e *Engine) AcquireConcurrencySlot(principal string) int {
	if e == nil || !e.cfg.Enabled {
		return 0
	}
	e.mu.Lock()
	ct, ok := e.concurrent[principal]
	if !ok {
		ct = &concurrentTrack{inflight: make(map[string]int)}
		e.concurrent[principal] = ct
	}
	e.mu.Unlock()

	ct.mu.Lock()
	defer ct.mu.Unlock()
	ct.inflight[principal]++
	return ct.inflight[principal]
}

// ReleaseConcurrencySlot releases a concurrent request slot.
func (e *Engine) ReleaseConcurrencySlot(principal string) {
	if e == nil || !e.cfg.Enabled {
		return
	}
	e.mu.RLock()
	ct, ok := e.concurrent[principal]
	e.mu.RUnlock()
	if !ok {
		return
	}
	ct.mu.Lock()
	defer ct.mu.Unlock()
	if ct.inflight[principal] > 0 {
		ct.inflight[principal]--
	}
}

// trigger creates an event and executes the disposition strategy.
func (e *Engine) trigger(ctx context.Context, principal, provider, model, clientIP, department string, at AnomalyType, severity Severity, observed, baseline float64, tokens int64, now time.Time, extraMsg string) {
	if e == nil {
		return
	}

	// Check cooldown
	lastAction, ok := e.cooldowns[principal]
	if ok && now.Sub(lastAction) < e.cfg.CooldownInterval {
		return
	}

	strategy, hasStrategy := e.cfg.Strategies[at]
	if !hasStrategy {
		return
	}

	multiplier := 0.0
	if baseline > 0 {
		multiplier = observed / baseline
	}

	msg := fmt.Sprintf("%s detected for %s: observed=%.1f baseline=%.1f (%.1fx)", at, principal, observed, baseline, multiplier)
	if extraMsg != "" {
		msg = msg + ": " + extraMsg
	}

	event := &Event{
		ID:             e.nextEventID(),
		AnomalyType:    at,
		Severity:       severity,
		Principal:      principal,
		Provider:       provider,
		Department:     department,
		ClientIP:       clientIP,
		Model:          model,
		Disposition:    strategy.Action,
		ObservedValue:  observed,
		BaselineValue:  baseline,
		Multiplier:     multiplier,
		TokensConsumed: tokens,
		Message:        msg,
		TriggeredAt:    now,
	}

	e.recordEvent(principal, event)
	e.cooldowns[principal] = now

	log.WithFields(log.Fields{
		"anomaly_type": at,
		"severity":     severity,
		"principal":    principal,
		"disposition":  strategy.Action,
		"observed":     observed,
		"baseline":     baseline,
		"multiplier":   multiplier,
		"tokens":       tokens,
		"event_id":     event.ID,
	}).Warn("anomaly detection triggered")

	if e.alertFn != nil {
		e.alertFn(ctx, event)
	}
}

// recordEvent stores an event in the in-memory ledger.
func (e *Engine) recordEvent(principal string, event *Event) {
	events := e.events[principal]
	if len(events) >= e.cfg.MaxEventsPerKey {
		events = events[len(events)-e.cfg.MaxEventsPerKey+1:]
	}
	e.events[principal] = append(events, event)
}

// Events returns all events for a given principal, or all events if principal is empty.
func (e *Engine) Events(principal string) []*Event {
	if e == nil {
		return nil
	}
	e.mu.RLock()
	defer e.mu.RUnlock()

	if principal != "" {
		events := e.events[principal]
		out := make([]*Event, len(events))
		copy(out, events)
		return out
	}

	var all []*Event
	for _, evts := range e.events {
		all = append(all, evts...)
	}
	return all
}

// RecentEvents returns events triggered within the given duration from now.
func (e *Engine) RecentEvents(ago time.Duration, now time.Time) []*Event {
	if e == nil {
		return nil
	}
	e.mu.RLock()
	defer e.mu.RUnlock()

	cutoff := now.Add(-ago)
	var out []*Event
	for _, evts := range e.events {
		for _, ev := range evts {
			if ev.TriggeredAt.After(cutoff) {
				out = append(out, ev)
			}
		}
	}
	return out
}

// Stats returns aggregate statistics for the given principal.
type Stats struct {
	Principal   string     `json:"principal"`
	CurrentTPM  float64    `json:"current_tpm"`
	CurrentQPS  float64    `json:"current_qps"`
	BaselineTPM float64    `json:"baseline_tpm"`
	BaselineQPS float64    `json:"baseline_qps"`
	EventCount  int        `json:"event_count"`
	LastEventAt *time.Time `json:"last_event_at,omitempty"`
}

// Stats returns aggregate statistics for the given principal.
func (e *Engine) Stats(principal, provider string, now time.Time) *Stats {
	if e == nil {
		return nil
	}
	e.mu.RLock()
	defer e.mu.RUnlock()

	key := e.windowKey(principal, provider)
	sw := e.windows[key]
	bl := e.baselines[key]

	s := &Stats{Principal: principal}
	if sw != nil {
		tpm := sw.TPM(now)
		qps := sw.QPS(now)
		// Use RLock-safe snapshot
		rawTokens, rawReqs := sw.Snapshot()
		_ = rawTokens
		_ = rawReqs
		s.CurrentTPM = tpm
		s.CurrentQPS = qps
	}
	if bl != nil {
		s.BaselineTPM = bl.avgTPM
		s.BaselineQPS = bl.avgQPS
	}

	evts := e.events[principal]
	s.EventCount = len(evts)
	if len(evts) > 0 {
		last := evts[len(evts)-1].TriggeredAt
		s.LastEventAt = &last
	}
	return s
}

// AllStats returns stats for all tracked principals.
func (e *Engine) AllStats(now time.Time) []*Stats {
	if e == nil {
		return nil
	}
	e.mu.RLock()
	defer e.mu.RUnlock()

	var out []*Stats
	for key := range e.windows {
		principal, provider := e.splitWindowKey(key)
		s := &Stats{
			Principal: principal,
		}
		sw := e.windows[key]
		bl := e.baselines[key]
		if sw != nil {
			s.CurrentTPM = sw.TPM(now)
			s.CurrentQPS = sw.QPS(now)
		}
		if bl != nil {
			s.BaselineTPM = bl.avgTPM
			s.BaselineQPS = bl.avgQPS
		}
		_ = provider
		evts := e.events[principal]
		s.EventCount = len(evts)
		if len(evts) > 0 {
			last := evts[len(evts)-1].TriggeredAt
			s.LastEventAt = &last
		}
		out = append(out, s)
	}
	return out
}

// ResolveEvent marks an event as resolved.
func (e *Engine) ResolveEvent(principal, eventID string, now time.Time) bool {
	if e == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	evts := e.events[principal]
	for _, ev := range evts {
		if ev.ID == eventID && ev.ResolvedAt == nil {
			ev.ResolvedAt = &now
			return true
		}
	}
	return false
}

func (e *Engine) windowKey(principal, provider string) string {
	return principal + "|" + provider
}

func (e *Engine) splitWindowKey(key string) (string, string) {
	for i := 0; i < len(key); i++ {
		if key[i] == '|' {
			return key[:i], key[i+1:]
		}
	}
	return key, ""
}

func (e *Engine) nextEventID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		e.nextID++
		return fmt.Sprintf("anom-%d", e.nextID)
	}
	return fmt.Sprintf("anom-%s", hex.EncodeToString(b))
}
