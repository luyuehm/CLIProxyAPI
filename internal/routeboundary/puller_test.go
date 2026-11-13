package routeboundary

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPullerSyncOnceAppliesBoundary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != boundaryEndpoint {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("X-CPA-Management-Key"); got != "secret" {
			t.Fatalf("unexpected management key: %q", got)
		}
		payload, _ := json.Marshal(boundaryResponse{Items: []boundaryRow{
			{APIKey: "sk-a", Enabled: true, AllowedProviders: []string{"openrouter", "anthropic"}, Strict: true},
			{APIKey: "sk-b", Enabled: false, AllowedProviders: []string{"claude"}, OverrideAllowed: true},
			// Rows with an empty apiKey are dropped, mirroring the policies
			// channel's puller.
			{APIKey: "", Enabled: true, AllowedProviders: []string{"gemini"}},
		}, Total: 3})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(payload)
	}))
	defer server.Close()

	store := NewStore()
	puller := NewPuller(store, PullerOptions{
		KeeperURL:     server.URL,
		ManagementKey: "secret",
		Interval:      time.Hour, // rely on SyncOnce, not the ticker
	})
	if err := puller.SyncOnce(context.Background()); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if store.Count() != 2 {
		t.Fatalf("expected 2 boundary rows, got %d", store.Count())
	}
	b, ok := store.Get("sk-a")
	if !ok {
		t.Fatal("expected boundary for sk-a")
	}
	if !b.Constrains() || !b.Strict {
		t.Fatalf("unexpected sk-a boundary: %+v", b)
	}
	if b.Allows("openrouter") != true || b.Allows("claude") {
		t.Fatalf("sk-a pool wrong: %v", b.AllowedProviders)
	}
}

// TestPullerFailureLatchesOldSnapshot covers the fail-open latch: a 5xx from
// KEEPER keeps the last good snapshot in place (routing behaviour does not
// flap during a control-plane outage).
func TestPullerFailureLatchesOldSnapshot(t *testing.T) {
	status := 200
	var items []boundaryRow
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		if status == http.StatusOK {
			payload, _ := json.Marshal(boundaryResponse{Items: items})
			_, _ = w.Write(payload)
		}
	}))
	defer server.Close()

	store := NewStore()
	puller := NewPuller(store, PullerOptions{
		KeeperURL:     server.URL,
		ManagementKey: "secret",
		Interval:      time.Hour,
	})

	// First a healthy delivery.
	items = []boundaryRow{{APIKey: "sk-a", Enabled: true, AllowedProviders: []string{"claude"}}}
	if err := puller.SyncOnce(context.Background()); err != nil {
		t.Fatalf("initial SyncOnce: %v", err)
	}
	if got := store.Count(); got != 1 {
		t.Fatalf("expected 1 boundary, got %d", got)
	}

	// Now the endpoint returns 503. The pull fails but the last good
	// snapshot is latched in place — the boundary keeps constraining traffic.
	status = http.StatusServiceUnavailable
	if err := puller.SyncOnce(context.Background()); err == nil {
		t.Fatal("SyncOnce should fail on 5xx")
	}
	if got := store.Count(); got != 1 {
		t.Fatalf("latch broken: expected 1 boundary after 5xx, got %d", got)
	}
	b, ok := store.Get("sk-a")
	if !ok || !b.Allows("claude") {
		t.Fatal("last good snapshot lost after failed pull")
	}

	// Transport failure (server down) also latches.
	server.Close()
	_ = puller.SyncOnce(context.Background())
	if _, ok := store.Get("sk-a"); !ok {
		t.Fatal("last good snapshot lost after transport failure")
	}
}

// TestPullerNeverVerifiedPassesThrough covers boot ordering: a KEEPER that
// comes up after the gateway leaves the store unverified, so the gate must
// stay inert until the first successful pull.
func TestPullerNeverVerifiedPassesThrough(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()

	store := NewStore()
	puller := NewPuller(store, PullerOptions{
		KeeperURL:     server.URL,
		ManagementKey: "secret",
		Interval:      time.Hour,
	})
	_ = puller.SyncOnce(context.Background())
	if store.Verified() {
		t.Fatal("store must stay unverified after a failed pull")
	}
	if _, ok := store.Get("sk-a"); ok {
		t.Fatal("unverified store must not serve boundaries")
	}
}

// TestStartStopLoop exercises the background ticker path deterministically:
// a 10ms interval pulls a few times, then Stop drains the loop without
// racing.
func TestPullerStartStopLoop(t *testing.T) {
	var pulls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pulls++
		payload, _ := json.Marshal(boundaryResponse{Items: []boundaryRow{
			{APIKey: "sk-a", Enabled: true, AllowedProviders: []string{"claude"}},
		}})
		_, _ = w.Write(payload)
	}))
	defer server.Close()

	store := NewStore()
	puller := NewPuller(store, PullerOptions{
		KeeperURL:     server.URL,
		ManagementKey: "secret",
		Interval:      10 * time.Millisecond,
	})
	puller.Start()
	time.Sleep(50 * time.Millisecond)
	puller.Stop()
	if pulls < 2 {
		t.Fatalf("expected the ticker loop to pull at least twice, got %d", pulls)
	}
	if store.Count() != 1 {
		t.Fatalf("expected synced boundary, got %d", store.Count())
	}
}
