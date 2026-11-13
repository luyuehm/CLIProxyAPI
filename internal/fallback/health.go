package fallback

import (
	"sync"
	"time"
)

// HealthWindow is the simple sliding-window failure tracker with exponential
// backoff that the orchestrator uses to keep a flapping provider out of the
// hot path. It is the issue's agreed handling for the "upstream flapping causes
// routing oscillation" rabbit hole: a bounded observation window plus a capped
// exponential cooldown, no consensus machinery.
//
// The window is per provider and driven entirely by caller-supplied timestamps
// (see Orchestrator's clock), so behaviour is deterministic and testable
// without wall-clock sleeps.
type HealthWindow struct {
	mu       sync.Mutex
	window   time.Duration
	base     time.Duration // cooldown for the first backoff step
	max      time.Duration // cooldown cap
	failures map[string]state
}

type state struct {
	// stamps holds failure timestamps inside the sliding window.
	stamps []time.Time
	// bursts counts consecutive window saturations; it drives the
	// exponential factor.
	bursts int
	// cooldownUntil is when the provider may be retried again.
	cooldownUntil time.Time
}

// NewHealthWindow builds a tracker. window is how far back failure events are
// counted; base is the first backoff cooldown; max caps it. Non-positive
// values keep the built-in defaults: a 5m window, a 5s base and a 5m cap.
func NewHealthWindow(window, base, max time.Duration) *HealthWindow {
	if window <= 0 {
		window = 5 * time.Minute
	}
	if base <= 0 {
		base = 5 * time.Second
	}
	if max <= 0 {
		max = 5 * time.Minute
	}
	return &HealthWindow{
		window:   window,
		base:     base,
		max:      max,
		failures: make(map[string]state),
	}
}

// ObserveSuccess clears a provider's failure state.
func (h *HealthWindow) ObserveSuccess(provider string, at time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.failures, provider)
}

// ObserveFailure records a failure at time. While the sliding window stays
// under 3 failures the provider is not cooled down; on the third the window
// saturates and a cooldown of base * 2^(bursts-1) (capped) is scheduled.
func (h *HealthWindow) ObserveFailure(provider string, at time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.failures[provider]
	st.stamps = append(st.stamps, at)
	st.prune(at, h.window)
	h.failures[provider] = st
	if len(st.stamps) >= 3 {
		// Window saturated: raise the burst count and schedule backoff.
		// Keep at most one saturated burst counted per window: reset the
		// stamps so the next saturation must rebuild the window, which is
		// what makes the backoff steps bounded and flapping-resistant.
		st.bursts++
		st.stamps = st.stamps[:0]
		cooldown := h.base
		for step := st.bursts - 1; step > 0; step-- {
			// Double with an overflow guard: test against max/2 so a huge
			// operator-supplied base cannot wrap negative (a negative
			// cooldown would fail-open the health gate).
			if cooldown > h.max/2 {
				cooldown = h.max
				break
			}
			cooldown *= 2
		}
		if cooldown > h.max {
			cooldown = h.max
		}
		st.cooldownUntil = at.Add(cooldown)
		h.failures[provider] = st
	}
}

// CooledDown reports whether the provider is inside a backoff cooldown at time,
// and when the cooldown ends. The zero value of a provider never is.
func (h *HealthWindow) CooledDown(provider string, at time.Time) (bool, time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.failures[provider]
	if !st.cooldownUntil.IsZero() && st.cooldownUntil.After(at) {
		return true, st.cooldownUntil
	}
	return false, time.Time{}
}

// FailuresInWindow reports how many failures are currently inside the
// sliding window for the provider. It is an observability hook.
func (h *HealthWindow) FailuresInWindow(provider string, at time.Time) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.failures[provider]
	pruned := st
	pruned.prune(at, h.window)
	return len(pruned.stamps)
}

// prune drops stamps older than the window. Caller must hold h.mu.
func (st *state) prune(at time.Time, window time.Duration) {
	cutoff := at.Add(-window)
	i := 0
	for i < len(st.stamps) && st.stamps[i].Before(cutoff) {
		i++
	}
	st.stamps = append(st.stamps[i:][:0], st.stamps[i:]...)
}
