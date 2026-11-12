package tracer

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

// newTestRecord builds a minimal valid usage record.
func newTestRecord(model string) coreusage.Record {
	return coreusage.Record{
		Model:       model,
		Provider:    "test-provider",
		RequestedAt: time.Now(),
		Detail:      coreusage.Detail{InputTokens: 1, OutputTokens: 2},
	}
}

// waitFor eventually returns true when cond is true or the timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

func TestReporterDeliversToKeeper(t *testing.T) {
	var mu sync.Mutex
	var gotPath, gotAuth string
	var received atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		mu.Lock()
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("X-CPA-Management-Key")
		mu.Unlock()
		received.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	reporter := NewReporter(ReporterConfig{
		KeeperURL:     server.URL,
		ManagementKey: "test-key",
	})
	reporter.Start()
	defer reporter.Stop()

	reporter.HandleUsage(context.Background(), newTestRecord("model-a"))
	waitFor(t, 3*time.Second, func() bool { return received.Load() == 1 })

	mu.Lock()
	defer mu.Unlock()
	if gotPath != defaultIngestPath {
		t.Fatalf("ingest path = %q, want %q", gotPath, defaultIngestPath)
	}
	if gotAuth != "test-key" {
		t.Fatalf("management key header = %q, want test-key", gotAuth)
	}
}

func TestReporterPayloadContract(t *testing.T) {
	var mu sync.Mutex
	var got IngestRequest
	var gotTraceparent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var decoded IngestRequest
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Errorf("ingest payload is not valid IngestRequest JSON: %v (body=%s)", err, body)
		}
		mu.Lock()
		got = decoded
		gotTraceparent = r.Header.Get("Traceparent")
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	reporter := NewReporter(ReporterConfig{KeeperURL: server.URL})
	reporter.Start()
	defer reporter.Stop()

	trace, _ := ParseTraceparent("00-0af7651912cd7359c451f5b756e546e8-c451f5b756e546e7-01")
	ctx := WithTraceContext(context.Background(), trace)
	reporter.HandleUsage(ctx, newTestRecord("model-b"))
	waitFor(t, 3*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return got.TraceID != ""
	})

	mu.Lock()
	spanID := got.Spans[0].ID
	traceparent := gotTraceparent
	spans := got.Spans
	traceID := got.TraceID
	mu.Unlock()

	if traceID != "0af7651912cd7359c451f5b756e546e8" {
		t.Fatalf("payload trace_id = %q, want adopted trace id", traceID)
	}
	if len(spans) != 1 {
		t.Fatalf("span count = %d, want exactly 1", len(spans))
	}
	if traceparent == "" {
		t.Fatal("adopted context must echo a traceparent header")
	}
	if spanID == "" {
		t.Fatal("span id must be present and deterministic")
	}
}

// TestReporterFailureGoesToRetry verifies the degradation contract: a down
// KEEPER must not block the producer, the report must land in the retry
// buffer, and must replay when KEEPER comes back — while the model call's
// own completion path is untouched (HandleUsage never blocks).
func TestReporterFailureGoesToRetry(t *testing.T) {
	// Phase 1: KEEPER down — reports accumulate in the retry buffer.
	reporter := NewReporter(ReporterConfig{
		KeeperURL:   "http://127.0.0.1:1", // unreachable (discard port)
		QueueSize:   4,
		RetryBuffer: 4,
	})
	// Use a short write timeout so the failed send returns fast.
	reporter.client = &http.Client{Timeout: 200 * time.Millisecond}
	reporter.Start()
	defer reporter.Stop()

	for i := 0; i < 2; i++ {
		reporter.HandleUsage(context.Background(), newTestRecord("model-c"))
	}
	// Give the worker a moment to attempt and fail both sends.
	waitFor(t, 5*time.Second, func() bool {
		_, _, _, _, failed := reporter.Stats()
		return failed >= 2
	})

	// Phase 2: KEEPER back — pending reports replay on the retry tick.
	var received atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		received.Add(1)
	}))
	defer server.Close()

	// Point the reporter at the live server and let the retry drain.
	reporter.SetIngestURL(server.URL + defaultIngestPath)
	waitFor(t, 10*time.Second, func() bool { return received.Load() >= 1 })

	enqueued, sent, dropped, retried, failed := reporter.Stats()
	if retried == 0 && sent == 0 {
		t.Fatalf("no replay after KEEPER recovered: enqueued=%d sent=%d dropped=%d retried=%d failed=%d",
			enqueued, sent, dropped, retried, failed)
	}
}

// TestReporterQueueFullDrops verifies the non-blocking contract: when the
// queue is full, HandleUsage drops the report instead of blocking, and the
// drop is counted.
func TestReporterQueueFullDrops(t *testing.T) {
	reporter := NewReporter(ReporterConfig{
		KeeperURL:   "http://127.0.0.1:1",
		QueueSize:   2,
		RetryBuffer: 2,
	})
	reporter.client = &http.Client{Timeout: time.Second}
	// Do NOT start the worker: the queue fills and stays full.
	for i := 0; i < 5; i++ {
		done := make(chan struct{})
		go func() {
			reporter.HandleUsage(context.Background(), newTestRecord("model-d"))
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("HandleUsage blocked on a full queue — it must be non-blocking")
		}
	}
	enqueued, _, dropped, _, _ := reporter.Stats()
	if enqueued != 2 {
		t.Fatalf("enqueued = %d, want 2 (queue capacity)", enqueued)
	}
	if dropped != 3 {
		t.Fatalf("dropped = %d, want 3", dropped)
	}
}

// TestReporterStopsAcceptsAfterStop verifies that a post-shutdown enqueue
// drops instead of panicking on the abandoned channels.
func TestReporterStopsAcceptsAfterStop(t *testing.T) {
	reporter := NewReporter(ReporterConfig{KeeperURL: "http://127.0.0.1:1"})
	reporter.Start()
	reporter.Stop()
	reporter.Stop() // idempotent
	// Must not panic or block after stop.
	reporter.HandleUsage(context.Background(), newTestRecord("model-e"))
}

// TestReporterHandlesDegenerateRecord verifies no-op on empty records.
func TestReporterHandlesDegenerateRecord(t *testing.T) {
	reporter := NewReporter(ReporterConfig{KeeperURL: "http://127.0.0.1:1"})
	reporter.HandleUsage(context.Background(), coreusage.Record{})
	enqueued, _, dropped, _, _ := reporter.Stats()
	if enqueued != 0 || dropped != 0 {
		t.Fatalf("degenerate record must be a no-op: enqueued=%d dropped=%d", enqueued, dropped)
	}
}
