package anomaly

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestSlidingWindowTPM(t *testing.T) {
	window := NewSlidingWindow(60*time.Second, 10)
	start := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	// Record 100 tokens per second for 10 seconds = 1000 tokens in window
	for i := 0; i < 10; i++ {
		window.Record(start.Add(time.Duration(i)*time.Second), 100, 1)
	}

	tpm := window.TPM(start.Add(10 * time.Second))
	want := 1000.0 // 1000 tokens / 1 minute = 1000 TPM
	if tpm < want-1 || tpm > want+1 {
		t.Errorf("TPM = %v, want approx %v", tpm, want)
	}

	qps := window.QPS(start.Add(10 * time.Second))
	// 10 requests / 60s = 0.166 QPS (full window denominator)
	if qps < 0.15 || qps > 0.18 {
		t.Errorf("QPS = %v, want approx 0.166", qps)
	}
}

func TestSlidingWindowEviction(t *testing.T) {
	window := NewSlidingWindow(60*time.Second, 10)
	start := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	window.Record(start, 500, 1)
	window.Record(start.Add(time.Second), 500, 1)

	tokens, reqs := window.Snapshot()
	if tokens != 1000 || reqs != 2 {
		t.Fatalf("before eviction: tokens=%d reqs=%d", tokens, reqs)
	}

	// Record after window has elapsed — should evict all old buckets
	window.Record(start.Add(120*time.Second), 100, 1)
	tokens, reqs = window.Snapshot()
	if tokens != 100 || reqs != 1 {
		t.Errorf("after eviction: tokens=%d reqs=%d, want 100/1", tokens, reqs)
	}
}

func TestEngineAbsoluteLimit(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.AbsoluteTPMLimit = 5000
	cfg.WindowSize = 60 * time.Second
	cfg.CooldownInterval = time.Hour

	engine := NewEngine(cfg)
	ctx := context.Background()

	var triggered []*Event
	var mu sync.Mutex
	engine.SetAlertFunc(func(_ context.Context, ev *Event) {
		mu.Lock()
		triggered = append(triggered, ev)
		mu.Unlock()
	})

	start := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	// 6 records × 1000 tokens = 6000 tokens in 60s window → TPM = 6000 > 5000
	for i := 0; i < 6; i++ {
		engine.RecordRequest(ctx, "key-abs", "openai", "gpt-5", "5.6.7.8", "", 1000, start.Add(time.Duration(i)*10*time.Second))
	}

	mu.Lock()
	hasEvent := len(triggered) > 0
	mu.Unlock()

	if !hasEvent {
		t.Errorf("expected absolute TPM limit event, got none")
	}
}

func TestEngineInfiniteLoopDetection(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.InfiniteLoopMinHits = 5
	cfg.InfiniteLoopWindow = time.Minute

	engine := NewEngine(cfg)

	start := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	for i := 0; i < 5; i++ {
		detected := engine.RecordInfiniteLoopCheck("key-loop", "openai", "hash-repeat", start.Add(time.Duration(i)*time.Second))
		if i < 4 && detected {
			t.Fatalf("loop detected too early at hit %d", i)
		}
		if i == 4 && !detected {
			t.Fatalf("loop not detected at hit 5")
		}
	}
}

func TestEngineConcurrencySlots(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enabled = true
	engine := NewEngine(cfg)

	principal := "key-conc"
	for i := 0; i < 5; i++ {
		count := engine.AcquireConcurrencySlot(principal)
		if count != i+1 {
			t.Errorf("acquire %d: got %d, want %d", i, count, i+1)
		}
	}
	for i := 0; i < 5; i++ {
		engine.ReleaseConcurrencySlot(principal)
	}
	if count := engine.AcquireConcurrencySlot(principal); count != 1 {
		t.Errorf("after release: got %d, want 1", count)
	}
}

func TestEngineCooldown(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.AbsoluteTPMLimit = 100
	cfg.CooldownInterval = time.Hour

	engine := NewEngine(cfg)
	ctx := context.Background()

	var triggerCount int
	var mu sync.Mutex
	engine.SetAlertFunc(func(_ context.Context, _ *Event) {
		mu.Lock()
		triggerCount++
		mu.Unlock()
	})

	start := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	engine.RecordRequest(ctx, "key-cd", "openai", "gpt-5", "", "", 1000, start)
	engine.RecordRequest(ctx, "key-cd", "openai", "gpt-5", "", "", 1000, start.Add(10*time.Second))

	mu.Lock()
	got := triggerCount
	mu.Unlock()

	if got > 1 {
		t.Errorf("expected cooldown to suppress second trigger, got %d events", got)
	}
}

func TestEngineEventsAndResolve(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.AbsoluteTPMLimit = 100
	cfg.CooldownInterval = time.Nanosecond // allow multiple triggers

	engine := NewEngine(cfg)
	ctx := context.Background()

	start := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	engine.RecordRequest(ctx, "key-ev", "openai", "gpt-5", "", "", 1000, start)
	engine.RecordRequest(ctx, "key-ev", "openai", "gpt-5", "", "", 1000, start.Add(time.Second))

	events := engine.Events("key-ev")
	if len(events) < 2 {
		t.Fatalf("expected at least 2 events, got %d", len(events))
	}

	idSet := make(map[string]struct{})
	for _, ev := range events {
		idSet[ev.ID] = struct{}{}
	}
	if len(idSet) != len(events) {
		t.Errorf("expected unique event IDs, got duplicates")
	}

	first := events[0]
	if !engine.ResolveEvent("key-ev", first.ID, start.Add(2*time.Second)) {
		t.Errorf("expected resolve to succeed")
	}
	if engine.ResolveEvent("key-ev", first.ID, start.Add(3*time.Second)) {
		t.Errorf("expected re-resolve to fail")
	}

	for _, ev := range events {
		if ev.ID == first.ID {
			if ev.ResolvedAt == nil {
				t.Errorf("expected event to be marked resolved")
			}
		}
	}
}

func TestEngineDisabled(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enabled = false
	cfg.AbsoluteTPMLimit = 100

	engine := NewEngine(cfg)
	ctx := context.Background()

	var triggered int
	var mu sync.Mutex
	engine.SetAlertFunc(func(_ context.Context, _ *Event) {
		mu.Lock()
		triggered++
		mu.Unlock()
	})

	start := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	engine.RecordRequest(ctx, "key-disabled", "openai", "gpt-5", "", "", 10000, start)

	mu.Lock()
	defer mu.Unlock()
	if triggered != 0 {
		t.Errorf("expected no events when disabled, got %d", triggered)
	}
}

func TestEngineStats(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enabled = true
	engine := NewEngine(cfg)

	start := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	engine.RecordRequest(context.Background(), "key-stats", "openai", "gpt-5", "", "", 100, start)

	stats := engine.Stats("key-stats", "openai", start.Add(time.Second))
	if stats == nil {
		t.Fatal("expected stats")
	}
	if stats.Principal != "key-stats" {
		t.Errorf("Principal = %q, want key-stats", stats.Principal)
	}

	all := engine.AllStats(start.Add(time.Second))
	if len(all) == 0 {
		t.Error("expected non-empty AllStats")
	}
}

func TestDefaultConfigValid(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.WindowSize <= 0 {
		t.Errorf("WindowSize should be positive, got %v", cfg.WindowSize)
	}
	if cfg.RateAnomalyThreshold <= 1.0 {
		t.Errorf("RateAnomalyThreshold should be > 1.0, got %v", cfg.RateAnomalyThreshold)
	}
	if cfg.NumBuckets <= 0 {
		t.Errorf("NumBuckets should be positive, got %d", cfg.NumBuckets)
	}
	for at, strategy := range cfg.Strategies {
		if strategy.Action == "" {
			t.Errorf("strategy for %s has empty action", at)
		}
	}
}

func TestEngineRecordWithNilEngine(t *testing.T) {
	var engine *Engine
	ctx := context.Background()
	start := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	// Should not panic
	engine.RecordRequest(ctx, "key", "openai", "gpt-5", "1.2.3.4", "eng", 100, start)
	engine.RecordInfiniteLoopCheck("key", "openai", "hash", start)
	engine.AcquireConcurrencySlot("key")
	engine.ReleaseConcurrencySlot("key")
	engine.ResolveEvent("key", "id", start)
	engine.Events("key")
	engine.RecentEvents(time.Minute, start)
	engine.Stats("key", "openai", start)
	engine.AllStats(start)
}

func TestEngineRecentEvents(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.AbsoluteTPMLimit = 100
	cfg.CooldownInterval = 0

	engine := NewEngine(cfg)
	ctx := context.Background()

	start := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	engine.RecordRequest(ctx, "key-re", "openai", "gpt-5", "", "", 1000, start)

	// Within 1 minute
	recent := engine.RecentEvents(1*time.Minute, start.Add(30*time.Second))
	if len(recent) == 0 {
		t.Errorf("expected recent events within 1 minute")
	}

	// Outside 1 microsecond
	noRecent := engine.RecentEvents(1*time.Microsecond, start.Add(30*time.Second))
	if len(noRecent) > 0 {
		t.Errorf("expected no events in 1us window, got %d", len(noRecent))
	}
}

func TestEngineAllEvents(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.AbsoluteTPMLimit = 100
	cfg.CooldownInterval = 0

	engine := NewEngine(cfg)
	ctx := context.Background()

	start := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	engine.RecordRequest(ctx, "key-a", "openai", "gpt-5", "", "", 1000, start)
	engine.RecordRequest(ctx, "key-b", "openai", "gpt-5", "", "", 1000, start.Add(time.Second))

	all := engine.Events("")
	if len(all) < 2 {
		t.Errorf("expected all events, got %d", len(all))
	}
}

func TestEngineResetWindow(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enabled = true
	engine := NewEngine(cfg)

	start := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	engine.RecordRequest(context.Background(), "key", "openai", "gpt-5", "", "", 100, start)
	sw := engine.windows["key|openai"]
	if sw == nil {
		t.Fatal("expected window to exist")
	}
	sw.Reset()
	tokens, reqs := sw.Snapshot()
	if tokens != 0 || reqs != 0 {
		t.Errorf("after reset: tokens=%d reqs=%d", tokens, reqs)
	}
}

func TestSlidingWindowEmpty(t *testing.T) {
	start := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	window := NewSlidingWindow(60*time.Second, 10)
	if tpm := window.TPM(start); tpm != 0 {
		t.Errorf("TPM on empty window: %v, want 0", tpm)
	}
	if qps := window.QPS(start); qps != 0 {
		t.Errorf("QPS on empty window: %v, want 0", qps)
	}
}

func TestSlidingWindowNil(t *testing.T) {
	var window *SlidingWindow
	start := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	// Should not panic
	window.Record(start, 100, 1)
	window.TPM(start)
	window.QPS(start)
	window.Reset()
	window.Snapshot()
}

func TestEngineWindowKey(t *testing.T) {
	cfg := DefaultConfig()
	engine := NewEngine(cfg)
	principal, provider := engine.splitWindowKey("test-key|openai")
	if principal != "test-key" {
		t.Errorf("principal = %q", principal)
	}
	if provider != "openai" {
		t.Errorf("provider = %q", provider)
	}
}

func TestEngineEventMaxEvents(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.AbsoluteTPMLimit = 1
	cfg.CooldownInterval = 0
	cfg.MaxEventsPerKey = 5

	engine := NewEngine(cfg)
	ctx := context.Background()

	start := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		engine.RecordRequest(ctx, "key-max", "openai", "gpt-5", "", "", 1000, start.Add(time.Duration(i)*time.Second))
	}

	events := engine.Events("key-max")
	if len(events) > 5 {
		t.Errorf("expected at most %d events, got %d", 5, len(events))
	}
}

func TestEngineConcurrentRecordings(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.AbsoluteTPMLimit = 1000
	cfg.CooldownInterval = 0

	engine := NewEngine(cfg)
	ctx := context.Background()

	start := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			engine.RecordRequest(ctx, "key-conc", "openai", "gpt-5", "", "", 100, start.Add(time.Duration(n)*time.Millisecond))
		}(i)
	}
	wg.Wait()

	events := engine.Events("key-conc")
	t.Logf("concurrent recordings produced %d events", len(events))
	// Just ensure no panic under concurrency
}
