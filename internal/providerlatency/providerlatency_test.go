package providerlatency

import (
	"testing"
	"time"
)

func TestColdSourceReportsNotOK(t *testing.T) {
	t0 := time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)
	w := NewWindow(Options{Now: func() time.Time { return t0 }})
	for i := 1; i < 3; i++ { // fewer than defaultMinSamples (5)
		w.Record("openai", int64(100*i), t0)
	}
	if _, ok := w.Percentile("openai", 99); ok {
		t.Errorf("cold source must report ok=false below min samples")
	}
	if w.HasData("openai") {
		t.Errorf("HasData = true on a cold source")
	}
}

func TestPercentileNearestRank(t *testing.T) {
	t0 := time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)
	advancing := t0
	w := NewWindow(Options{Now: func() time.Time { return advancing }})
	values := []int64{100, 200, 300, 400, 500}
	for i, v := range values {
		w.Record("openai", v, t0.Add(time.Duration(i)*time.Second))
		advancing = t0.Add(time.Duration(i+1) * time.Second)
	}
	// 99th percentile of {100..500} (5 samples): rank = ceil(5*99/100)-1 = 4 -> 500.
	if ms, ok := w.Percentile("openai", 99); !ok || ms != 500 {
		t.Errorf("p99 = %d, ok=%v; want 500, true", ms, ok)
	}
	// p50: rank = ceil(5*50/100)-1 = 2 -> 300.
	if ms, ok := w.Percentile("openai", 50); !ok || ms != 300 {
		t.Errorf("p50 = %d, ok=%v; want 300, true", ms, ok)
	}
}

func TestWindowEvictsStaleSamples(t *testing.T) {
	base := time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)
	now := base
	w := NewWindow(Options{Window: time.Minute, Now: func() time.Time { return now }})
	// 4 stale samples (2h ago, outside the 1m window).
	for i := 0; i < 4; i++ {
		w.Record("openai", int64(100+i), base.Add(-2*time.Hour))
	}
	if _, ok := w.Percentile("openai", 99); ok {
		t.Errorf("stale samples must be evicted, got ok")
	}
	now = base // clock advances: all records are now 2h old relative to window
	for i := 0; i < 5; i++ {
		w.Record("openai", int64(100+i), now.Add(-time.Second))
	}
	now = now.Add(2 * time.Hour) // past the window for both batches
	if _, ok := w.Percentile("openai", 99); ok {
		t.Errorf("all samples out of window; percentile must be ok=false")
	}
}

func TestRecordIgnoresInvalid(t *testing.T) {
	now := time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)
	w := NewWindow(Options{Now: func() time.Time { return now }})
	w.Record("", 10, now)
	w.Record("openai", 0, now)
	w.Record("openai", -1, now)
	for i := 0; i < 4; i++ {
		w.Record("openai", 10, now)
	}
	if _, ok := w.Percentile("openai", 99); ok {
		t.Errorf("only 4 valid samples -> still cold, must be ok=false")
	}
}
