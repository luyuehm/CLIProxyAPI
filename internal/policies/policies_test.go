package policies

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// newTestRouter builds a Gin engine with a passthrough auth stub that sets the
// userApiKey context value from the Authorization header, then mounts the
// middleware under test.
func newTestRouter(mw gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		key := extractBearer(c.GetHeader("Authorization"))
		if key != "" {
			c.Set("userApiKey", key)
		}
		c.Next()
	})
	router.Use(mw)
	router.Any("/*path", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})
	return router
}

func serveRequest(router http.Handler, req *http.Request) (int, string) {
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	return resp.Code, resp.Body.String()
}

func TestStoreApplyAndGet(t *testing.T) {
	store := NewStore()
	if store.Verified() {
		t.Fatal("new store should not be verified")
	}
	store.Apply([]Policy{
		{APIKey: "sk-a", RPMLimit: 60, MaxConcurrent: 5},
	})
	if !store.Verified() {
		t.Fatal("store should be verified after Apply")
	}
	p, ok := store.Get("sk-a")
	if !ok {
		t.Fatal("expected policy for sk-a")
	}
	if p.RPMLimit != 60 || p.MaxConcurrent != 5 {
		t.Fatalf("unexpected policy: %+v", p)
	}
	if _, ok := store.Get("sk-missing"); ok {
		t.Fatal("unexpected policy for sk-missing")
	}
	if store.Count() != 1 {
		t.Fatalf("expected count 1, got %d", store.Count())
	}
}

func TestStoreApplyReplacesSnapshot(t *testing.T) {
	store := NewStore()
	store.Apply([]Policy{{APIKey: "sk-a", RPMLimit: 1}})
	store.Apply([]Policy{{APIKey: "sk-b", RPMLimit: 2}})
	if _, ok := store.Get("sk-a"); ok {
		t.Fatal("sk-a should be gone after snapshot replacement")
	}
	if _, ok := store.Get("sk-b"); !ok {
		t.Fatal("sk-b should be present")
	}
}

func TestPullerSyncOnceAppliesPolicy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != policyEndpoint {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("X-CPA-Management-Key"); got != "secret" {
			t.Fatalf("unexpected management key: %q", got)
		}
		payload, _ := json.Marshal(policyResponse{Items: []policyRow{
			{APIKey: "sk-a", Enabled: true, RPMLimit: 60, MaxConcurrent: 5},
			{APIKey: "sk-b", Enabled: false, RPMLimit: 0},
		}, Total: 2})
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
		t.Fatalf("SyncOnce returned error: %v", err)
	}
	p, ok := store.Get("sk-a")
	if !ok {
		t.Fatal("expected policy for sk-a")
	}
	if p.RPMLimit != 60 || p.MaxConcurrent != 5 || !p.Enabled {
		t.Fatalf("unexpected policy: %+v", p)
	}
	if _, ok := store.Get("sk-b"); !ok {
		t.Fatal("expected policy for sk-b (revoked/disabled keys still delivered)")
	}
	if pulls, failures := puller.Stats(); pulls != 1 || failures != 0 {
		t.Fatalf("unexpected stats: pulls=%d failures=%d", pulls, failures)
	}
}

func TestPullerFailedPullLatchesSnapshot(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			payload, _ := json.Marshal(policyResponse{Items: []policyRow{
				{APIKey: "sk-a", RPMLimit: 10},
			}, Total: 1})
			_, _ = w.Write(payload)
			return
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	store := NewStore()
	puller := NewPuller(store, PullerOptions{KeeperURL: server.URL, ManagementKey: "secret"})
	_ = puller.SyncOnce(context.Background())
	if err := puller.SyncOnce(context.Background()); err == nil {
		t.Fatal("expected second SyncOnce to fail")
	}
	// The failed pull must latch the first snapshot.
	if p, ok := store.Get("sk-a"); !ok || p.RPMLimit != 10 {
		t.Fatalf("snapshot not latched after failure: ok=%v p=%+v", ok, p)
	}
}

func TestMiddlewareEnforcesRPM(t *testing.T) {
	store := NewStore()
	store.Apply([]Policy{{APIKey: "sk-a", RPMLimit: 2}})
	m := NewMiddleware(MiddlewareOptions{Store: store})

	router := newTestRouter(m.Handler())
	// First two requests pass, third is 429.
	req1 := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
	req1.Header.Set("Authorization", "Bearer sk-a")
	if code, _ := serveRequest(router, req1); code != http.StatusOK {
		t.Fatalf("first request: expected 200, got %d", code)
	}
	req2 := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
	req2.Header.Set("Authorization", "Bearer sk-a")
	if code, _ := serveRequest(router, req2); code != http.StatusOK {
		t.Fatalf("second request: expected 200, got %d", code)
	}
	req3 := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
	req3.Header.Set("Authorization", "Bearer sk-a")
	code, resp := serveRequest(router, req3)
	if code != http.StatusTooManyRequests {
		t.Fatalf("third request: expected 429, got %d body=%s", code, resp)
	}
}

func TestMiddlewareEnforcesConcurrency(t *testing.T) {
	store := NewStore()
	store.Apply([]Policy{{APIKey: "sk-a", MaxConcurrent: 1}})
	m := NewMiddleware(MiddlewareOptions{Store: store})

	// Simulate two concurrent in-flight requests to /v1/chat/completions.
	// The concurrency counter is bumped on entry and released on exit from
	// the deferred Release, so a single sequential request sees count 1→0.
	// To observe the guard we need overlap, but the gate itself is exercised
	// by bumping the counter directly first.
	req := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-a")
	router := newTestRouter(m.Handler())

	// Pre-bump the concurrency to simulate an in-flight request.
	m.bumpConcurrency("sk-a", 1)

	// Now a new request should be rejected (concurrency already at limit).
	code, _ := serveRequest(router, req)
	if code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 when concurrency at limit, got %d", code)
	}

	// Release and the next request passes.
	m.bumpConcurrency("sk-a", -1)
	if code, _ := serveRequest(router, req); code != http.StatusOK {
		t.Fatalf("expected 200 after concurrency released, got %d", code)
	}
}

func TestDeriveKeeperURL(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"http://127.0.0.1:8317", "http://127.0.0.1:4320"},
		{"https://cpa.example.com:8317", "https://cpa.example.com:4320"},
		{"http://cpa:8317/", "http://cpa:4320"},
	}
	for _, tc := range cases {
		if got := deriveKeeperURL(tc.in); got != tc.want {
			t.Fatalf("deriveKeeperURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestMiddlewareIgnoresNonDataPlane(t *testing.T) {
	store := NewStore()
	store.Apply([]Policy{{APIKey: "sk-a", RPMLimit: 1}})
	m := NewMiddleware(MiddlewareOptions{Store: store})
	router := newTestRouter(m.Handler())

	// Healthz is outside the guarded scopes — never rate limited.
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Authorization", "Bearer sk-a")
	if code, _ := serveRequest(router, req); code != http.StatusOK {
		t.Fatalf("healthz should not be rate limited, got %d", code)
	}
}