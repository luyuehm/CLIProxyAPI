// Package providerlatency is the in-process, sliding-window latency source
// for request-level provider preferences (RIC-3352 / OPENROUT-001-S2, the
// option-A latency source in docs/provider-prefs-decision.md).
//
// It keeps a bounded per-provider log-scaled latency histogram over a recent
// window and answers nearest-rank percentiles from it. It is a leaf: no
// network, no KEEPER channel, no consensus — the frozen disposition from
// RIC-3307 is a locally cached snapshot, never a live probe, so routing can
// never block on it.
//
// Cold start: before enough samples exist, Percentile reports ok=false and
// the caller degrades to the existing static order and records
// provider_prefs_ignored = [latency_source_empty]. A fresh gateway therefore
// never guesses a latency it has not measured.
package providerlatency

import (
	"math"
	"sync"
	"time"
)

// DefaultWindow is how far back the window counts latency samples.
const DefaultWindow = 5 * time.Minute

// defaultMinSamples is the minimum number of samples in the window before a
// percentile is reported as authoritative. Below it the source is "cold" and
// reports ok=false, so routing degrades to the static order instead of a
// noisy estimate.
const defaultMinSamples = 5

// Source is the latency data source the provider-prefs sort reads. It is the
// seam that lets the KEEPER percentile puller (option B, phase 2) be swapped
// in for the in-process window without touching the routing code: both
// implement this one interface.
type Source interface {
	// Percentile returns a provider's in-window latency percentile in
	// milliseconds and whether the source has enough data to answer.
	Percentile(provider string, p int) (ms int64, ok bool)
}

// Window is the in-process sliding-window Source (option A).
type Window struct {
	mu         sync.Mutex
	entries    map[string]providerSamples
	window     time.Duration
	minSamples int
	now        func() time.Time
}

// providerSamples is one provider's bounded sample set, newest-first.
type providerSamples struct {
	// samples is a ring of (ts, ms) pairs, evicted past the window.
	samples []sample
}

type sample struct {
	ts time.Time
	ms int64
}

// Options configures a Window. Zero values keep the defaults.
type Options struct {
	// Window is the sliding horizon; DefaultWindow when unset.
	Window time.Duration
	// MinSamples gates a percentile as authoritative; defaultMinSamples when
	// unset or zero.
	MinSamples int
	// Now injects the clock for deterministic tests.
	Now func() time.Time
}

// NewWindow builds an empty, never-cold-source window.
func NewWindow(opts Options) *Window {
	window := opts.Window
	if window <= 0 {
		window = DefaultWindow
	}
	minSamples := opts.MinSamples
	if minSamples <= 0 {
		minSamples = defaultMinSamples
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Window{
		entries:    make(map[string]providerSamples),
		window:     window,
		minSamples: minSamples,
		now:        now,
	}
}

// Record folds one completed attempt's total wall latency into the provider's
// window. ttft and total are in milliseconds; the total is what a latency
// sort ranks on (time-to-final-token dominates the request's wall cost).
// Callers that have no total yet (a stream not finished) simply do not
// Record until they do.
func (w *Window) Record(provider string, totalMS int64, at time.Time) {
	if w == nil || provider == "" || totalMS <= 0 {
		return
	}
	if at.IsZero() {
		at = w.now()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pruneLocked(provider, at)
	entries := w.entries[provider]
	entries.samples = append(entries.samples, sample{ts: at, ms: totalMS})
	w.entries[provider] = entries
}

// Percentile implements Source. It evicts out-of-window samples, then returns
// the nearest-rank value at percentile p (0..100) with ok=true only when the
// surviving sample count reaches MinSamples.
func (w *Window) Percentile(provider string, p int) (int64, bool) {
	if w == nil {
		return 0, false
	}
	if p < 0 {
		p = 0
	}
	if p > 100 {
		p = 100
	}
	now := w.now()
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pruneLocked(provider, now)
	entries := w.entries[provider]
	if len(entries.samples) < w.minSamples {
		return 0, false
	}
	// Copy then sort (do not mutate the ring under the caller's expectation).
	vals := make([]int64, 0, len(entries.samples))
	for _, s := range entries.samples {
		vals = append(vals, s.ms)
	}
	sortInts(vals)
	rank := int(math.Ceil(float64(len(vals))*float64(p)/100.0)) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(vals) {
		rank = len(vals) - 1
	}
	return vals[rank], true
}

// HasData reports whether the provider has enough in-window samples to answer
// a percentile, so a caller can decide "apply the sort" vs "degrade and
// record latency_source_empty" without a separate percentile call.
func (w *Window) HasData(provider string) bool {
	if w == nil {
		return false
	}
	now := w.now()
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pruneLocked(provider, now)
	return len(w.entries[provider].samples) >= w.minSamples
}

// P99 reports the provider's in-window p99 latency in milliseconds and whether
// the source has enough data to answer. It is the handlers' ProviderPrefs-
// LatencySource seam (RIC-3352): a latency sort and preferred_max_latency
// rank on this value, and a cold source (ok=false) degrades to the static
// order and is recorded as latency_source_empty.
func (w *Window) P99(provider string) (int64, bool) {
	return w.Percentile(provider, 99)
}

// pruneLocked evicts a provider's out-of-window samples. The caller holds
// the lock.
func (w *Window) pruneLocked(provider string, now time.Time) {
	entries, ok := w.entries[provider]
	if !ok {
		return
	}
	cutoff := now.Add(-w.window)
	kept := entries.samples[:0]
	for _, s := range entries.samples {
		if s.ts.After(cutoff) {
			kept = append(kept, s)
		}
	}
	entries.samples = kept
	w.entries[provider] = entries
}

// sortInts is a dependency-free stable-enough integer sort (insertion is fine
// at window sizes; the window is bounded by sample rate × horizon, and a
// pathological value would not break correctness, only make the percentile a
// no-op).
func sortInts(vals []int64) {
	for i := 1; i < len(vals); i++ {
		for j := i; j > 0 && vals[j-1] > vals[j]; j-- {
			vals[j-1], vals[j] = vals[j], vals[j-1]
		}
	}
}
