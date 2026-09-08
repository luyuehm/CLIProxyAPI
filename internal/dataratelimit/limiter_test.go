package dataratelimit

import (
	"testing"
	"time"
)

// newTestLimiter builds a limiter with a controllable clock starting at epoch
// so tests can advance time deterministically.
func newTestLimiter(t *testing.T, opts LimiterOptions) (*Limiter, *time.Time) {
	t.Helper()
	base := time.Unix(0, 0)
	now := base
	opts.Now = func() time.Time { return now }
	if opts.MaxEntries == 0 {
		opts.MaxEntries = 8
	}
	l := NewLimiter(opts)
	return l, &now
}

func advanceClock(now *time.Time, d time.Duration) {
	*now = now.Add(d)
}

func TestLimiterAllowsWithinRPM(t *testing.T) {
	l, _ := newTestLimiter(t, LimiterOptions{DefaultRPM: 3})
	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("key-a"); !ok {
			t.Fatalf("request %d within RPM should be allowed", i+1)
		}
		// Simulate request completion so concurrency is released.
		l.Release("key-a")
	}
}

func TestLimiterRejectsAfterRPM(t *testing.T) {
	l, _ := newTestLimiter(t, LimiterOptions{DefaultRPM: 2})
	for i := 0; i < 2; i++ {
		if ok, _ := l.Allow("key-a"); !ok {
			t.Fatalf("request %d should be allowed", i+1)
		}
		l.Release("key-a")
	}
	if ok, retryAfter := l.Allow("key-a"); ok {
		t.Fatal("fourth request should exceed RPM and be rejected")
	} else if retryAfter <= 0 {
		t.Fatalf("expected positive Retry-After, got %v", retryAfter)
	}
}

func TestLimiterRefillsTokensAfterWindow(t *testing.T) {
	l, now := newTestLimiter(t, LimiterOptions{DefaultRPM: 60})
	for i := 0; i < 60; i++ {
		l.Allow("key-a")
		l.Release("key-a")
	}
	if ok, _ := l.Allow("key-a"); ok {
		t.Fatal("bucket should be empty after 60 requests")
	}
	// Advance 30 seconds — 30 tokens refill at 60 RPM.
	advanceClock(now, 30*time.Second)
	for i := 0; i < 30; i++ {
		if ok, _ := l.Allow("key-a"); !ok {
			t.Fatalf("request %d after 30s refill should be allowed", i+1)
		}
		l.Release("key-a")
	}
	if ok, _ := l.Allow("key-a"); ok {
		t.Fatal("bucket should be empty again after refill consumed")
	}
}

func TestLimiterConcurrencyCeiling(t *testing.T) {
	l, _ := newTestLimiter(t, LimiterOptions{DefaultConcurrency: 2})
	if ok, _ := l.Allow("key-a"); !ok {
		t.Fatal("first concurrent request should be allowed")
	}
	if ok, _ := l.Allow("key-a"); !ok {
		t.Fatal("second concurrent request should be allowed")
	}
	if ok, _ := l.Allow("key-a"); ok {
		t.Fatal("third concurrent request should be rejected")
	}
	// Releasing one slot frees it for the next request.
	l.Release("key-a")
	if ok, _ := l.Allow("key-a"); !ok {
		t.Fatal("request after release should be allowed")
	}
}

func TestLimiterKeysAreIsolated(t *testing.T) {
	l, _ := newTestLimiter(t, LimiterOptions{DefaultRPM: 1})
	if ok, _ := l.Allow("key-a"); !ok {
		t.Fatal("key-a first should be allowed")
	}
	l.Release("key-a")
	if ok, _ := l.Allow("key-a"); ok {
		t.Fatal("key-a should be exhausted")
	}
	if ok, _ := l.Allow("key-b"); !ok {
		t.Fatal("key-b should be independent of key-a")
	}
}

func TestLimiterAllowWithLimitsOverridesDefaults(t *testing.T) {
	l, _ := newTestLimiter(t, LimiterOptions{DefaultRPM: 100})
	// Explicit RPM of 1 should cap at 1, not default 100.
	if ok, _ := l.AllowWithLimits("key-a", KeyLimits{RPM: 1}); !ok {
		t.Fatal("first request should pass")
	}
	l.Release("key-a")
	if ok, _ := l.AllowWithLimits("key-a", KeyLimits{RPM: 1}); ok {
		t.Fatal("second request should exceed explicit RPM 1")
	}
}

func TestLimiterZeroLimitsMeansUnlimited(t *testing.T) {
	l, _ := newTestLimiter(t, LimiterOptions{DefaultRPM: 0, DefaultConcurrency: 0})
	for i := 0; i < 1000; i++ {
		if ok, _ := l.Allow("key-a"); !ok {
			t.Fatalf("request %d should be allowed with no limits", i+1)
		}
	}
}

func TestLimiterEvictsIdleBucketsAtCap(t *testing.T) {
	l, now := newTestLimiter(t, LimiterOptions{DefaultRPM: 1, MaxEntries: 4})
	for i := 0; i < 4; i++ {
		key := "key-" + string(rune('a'+i))
		l.Allow(key)
		l.Release(key)
	}
	// Aging: after idleTTL all four buckets are idle and evictable.
	advanceClock(now, time.Hour)
	// Fifth key forces eviction of the oldest idle bucket.
	l.Allow("key-e")
	l.Release("key-e")
	if len(l.buckets) > 4 {
		t.Fatalf("expected at most %d buckets after eviction, got %d", 4, len(l.buckets))
	}
}

func TestLimiterReleaseIsIdempotent(t *testing.T) {
	l, _ := newTestLimiter(t, LimiterOptions{DefaultConcurrency: 1})
	if ok, _ := l.Allow("key-a"); !ok {
		t.Fatal("request should be allowed")
	}
	// Double release should not go negative.
	l.Release("key-a")
	l.Release("key-a")
	if got := l.InFlight("key-a"); got != 0 {
		t.Fatalf("expected in-flight 0 after double release, got %d", got)
	}
}
