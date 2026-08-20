package alerts

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

const (
	// maxEvents caps the in-memory event history returned by the management API.
	maxEvents = 200
	// minRequestsForRate guards the anomaly error-rate rule against a tiny
	// denominator, which would otherwise turn one failed request into a 100%
	// error rate.
	minRequestsForRate = 10
)

// sample is one usage record folded into a rolling window.
type sample struct {
	ts       time.Time
	tokens   int64
	requests int64
	errors   int64
}

// windowStats aggregates samples for one usage target.
type windowStats struct {
	samples  []sample
	tokens   int64
	requests int64
	errors   int64
}

func (w *windowStats) add(record coreusage.Record, now time.Time) {
	var errs int64
	if record.Failed {
		errs = 1
	}
	w.samples = append(w.samples, sample{ts: now, tokens: record.Detail.TotalTokens, requests: 1, errors: errs})
	w.tokens += record.Detail.TotalTokens
	w.requests++
	w.errors += errs
}

func (w *windowStats) prune(now time.Time, window time.Duration) {
	cutoff := now.Add(-window)
	kept := w.samples[:0]
	for _, s := range w.samples {
		if s.ts.Before(cutoff) {
			w.tokens -= s.tokens
			w.requests -= s.requests
			w.errors -= s.errors
			continue
		}
		kept = append(kept, s)
	}
	w.samples = kept
}

// Manager aggregates usage records, evaluates alert rules on a fixed interval,
// and pushes fired alerts to the configured webhooks. It implements
// coreusage.Plugin so the usage accounting pipeline dispatches records to it.
type Manager struct {
	mu       sync.Mutex
	cfg      Config
	interval time.Duration
	dispatch *dispatcher
	stats    map[string]*windowStats
	fired    map[string]time.Time
	events   []Event

	startOnce sync.Once
	cancel    context.CancelFunc
	done      chan struct{}
}

// NewManager constructs a Manager from config. Call Start to begin the
// evaluation loop.
func NewManager(cfg Config) *Manager {
	cfg = cfg.Normalized()
	return &Manager{
		cfg:      cfg,
		interval: cfg.Interval(),
		dispatch: newDispatcher(cfg),
		stats:    make(map[string]*windowStats),
		fired:    make(map[string]time.Time),
	}
}

// Enabled reports whether the manager is enabled.
func (m *Manager) Enabled() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg.Enabled
}

// Start launches the background evaluation loop. Calling Start multiple times
// is safe; only the first call takes effect.
func (m *Manager) Start(ctx context.Context) {
	if m == nil {
		return
	}
	m.startOnce.Do(func() {
		if ctx == nil {
			ctx = context.Background()
		}
		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})

		m.mu.Lock()
		m.cancel = cancel
		m.done = done
		m.mu.Unlock()

		go m.run(runCtx, done)
	})
}

// Stop terminates the evaluation loop. It is a no-op when Start was never
// called.
func (m *Manager) Stop() {
	if m == nil {
		return
	}
	m.mu.Lock()
	cancel := m.cancel
	done := m.done
	m.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (m *Manager) run(ctx context.Context, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.evaluate()
		}
	}
}

// HandleUsage implements coreusage.Plugin. It folds each usage record into the
// rolling window for its target.
func (m *Manager) HandleUsage(_ context.Context, record coreusage.Record) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.cfg.Enabled {
		return
	}

	now := time.Now()
	target := targetFor(record)
	stats := m.stats[target]
	if stats == nil {
		stats = &windowStats{}
		m.stats[target] = stats
	}
	stats.add(record, now)
	stats.prune(now, m.interval)
}

// SendTextTo pushes a raw message to one configured channel. It is the entry
// point for the management API test endpoint.
func (m *Manager) SendTextTo(ctx context.Context, kind ChannelKind, text string) error {
	if m == nil {
		return fmt.Errorf("alerts: manager unavailable")
	}
	return m.dispatch.SendText(ctx, kind, text)
}

// Snapshot returns the current config, enabled channels, and recent events.
func (m *Manager) Snapshot() Snapshot {
	if m == nil {
		return Snapshot{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	return Snapshot{
		Enabled:       m.cfg.Enabled,
		CheckInterval: m.cfg.CheckInterval,
		Channels:      m.dispatch.EnabledChannels(),
		Rules:         append([]Rule(nil), m.cfg.Rules...),
		Events:        append([]Event(nil), m.events...),
	}
}

func (m *Manager) evaluate() {
	fired := m.evaluateLocked()
	for _, event := range fired {
		m.dispatch.Send(context.Background(), event)
	}
}

func (m *Manager) evaluateLocked() []Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.cfg.Enabled {
		return nil
	}

	now := time.Now()
	for target, stats := range m.stats {
		stats.prune(now, m.interval)
		if stats.requests == 0 {
			delete(m.stats, target)
		}
	}

	var fired []Event
	for _, rule := range m.cfg.Rules {
		if !rule.valid() {
			continue
		}
		for target, stats := range m.stats {
			if rule.Target != "" && rule.Target != target {
				continue
			}
			event, ok := m.evaluateRule(rule, target, stats, now)
			if !ok {
				continue
			}
			key := rule.Name + "\x00" + target
			cooldown := ruleCooldown(rule, m.interval)
			if last, seen := m.fired[key]; seen && now.Sub(last) < cooldown {
				continue
			}
			m.fired[key] = now
			fired = append(fired, event)
		}
	}

	for _, event := range fired {
		m.events = append([]Event{event}, m.events...)
	}
	if len(m.events) > maxEvents {
		m.events = m.events[:maxEvents]
	}
	return fired
}

func (m *Manager) evaluateRule(rule Rule, target string, stats *windowStats, now time.Time) (Event, bool) {
	switch rule.Kind {
	case RuleUsageLimit:
		switch {
		case rule.TokenLimit > 0 && stats.tokens >= rule.TokenLimit:
			return m.newEvent(rule, target, stats, now,
				fmt.Sprintf("usage limit reached: %d tokens in window (limit %d)", stats.tokens, rule.TokenLimit)), true
		case rule.RequestLimit > 0 && stats.requests >= rule.RequestLimit:
			return m.newEvent(rule, target, stats, now,
				fmt.Sprintf("usage limit reached: %d requests in window (limit %d)", stats.requests, rule.RequestLimit)), true
		}
	case RuleFault:
		if rule.ErrorCountLimit > 0 && stats.errors >= rule.ErrorCountLimit {
			return m.newEvent(rule, target, stats, now,
				fmt.Sprintf("fault detected: %d errors in window (limit %d)", stats.errors, rule.ErrorCountLimit)), true
		}
	case RuleAnomaly:
		if rule.ErrorRateLimit > 0 && stats.requests >= minRequestsForRate {
			rate := float64(stats.errors) / float64(stats.requests)
			if rate >= rule.ErrorRateLimit {
				return m.newEvent(rule, target, stats, now,
					fmt.Sprintf("anomaly detected: %.1f%% error rate in window (limit %.1f%%)", rate*100, rule.ErrorRateLimit*100)), true
			}
		}
	}
	return Event{}, false
}

func (m *Manager) newEvent(rule Rule, target string, stats *windowStats, now time.Time, message string) Event {
	var rate float64
	if stats.requests > 0 {
		rate = float64(stats.errors) / float64(stats.requests)
	}
	return Event{
		Time:      now,
		Rule:      rule.Name,
		Kind:      rule.Kind,
		Severity:  rule.Severity,
		Target:    target,
		Message:   message,
		Tokens:    stats.tokens,
		Requests:  stats.requests,
		Errors:    stats.errors,
		ErrorRate: rate,
	}
}

func ruleCooldown(rule Rule, fallback time.Duration) time.Duration {
	if duration := parseDuration(rule.Cooldown, 0); duration > 0 {
		return duration
	}
	return fallback
}

func targetFor(record coreusage.Record) string {
	for _, candidate := range []string{record.AuthIndex, record.APIKey, record.Source} {
		if target := strings.TrimSpace(candidate); target != "" {
			return target
		}
	}
	return "unknown"
}
