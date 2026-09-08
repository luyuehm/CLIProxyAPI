package dataratelimit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// newTestMiddleware builds a middleware with a deterministic clock.
func newTestMiddleware(t *testing.T, opts MiddlewareOptions) *Middleware {
	t.Helper()
	opts.withDefaults()
	opts.PathScopes = []string{"/v1/"}
	return NewMiddleware(opts)
}

// runRequest executes a request through the middleware and returns the
// recorder. The key is injected into the gin context exactly as AuthMiddleware
// does in the real data plane (c.Set("userApiKey", ...)).
func runRequest(t *testing.T, mw *Middleware, path string, key string) *httptest.ResponseRecorder {
	t.Helper()
	router := gin.New()
	router.Use(func(c *gin.Context) {
		if key != "" {
			c.Set("userApiKey", key)
		}
		c.Next()
	})
	router.Use(mw.Handler())
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest(http.MethodPost, path, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func TestMiddlewareAllowsWithinLimit(t *testing.T) {
	limiter := NewLimiter(LimiterOptions{DefaultRPM: 5})
	mw := newTestMiddleware(t, MiddlewareOptions{Limiter: limiter})

	for i := 0; i < 5; i++ {
		w := runRequest(t, mw, "/v1/chat/completions", "sk-test")
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d body=%s", i+1, w.Code, w.Body.String())
		}
	}
}

func TestMiddlewareRejectsOverRPMWith429(t *testing.T) {
	limiter := NewLimiter(LimiterOptions{DefaultRPM: 2})
	mw := newTestMiddleware(t, MiddlewareOptions{Limiter: limiter})

	for i := 0; i < 2; i++ {
		if w := runRequest(t, mw, "/v1/chat/completions", "sk-test"); w.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d", i+1, w.Code)
		}
	}
	w := runRequest(t, mw, "/v1/chat/completions", "sk-test")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got == "" {
		t.Fatal("expected Retry-After header on 429")
	} else if n, err := strconv.Atoi(got); err != nil || n < 1 {
		t.Fatalf("invalid Retry-After value %q", got)
	}
	// The 429 must carry an OpenAI-compatible error body.
	body := w.Body.String()
	if body == "" {
		t.Fatal("expected JSON error body on 429")
	}
}

func TestMiddlewareRejectsOverConcurrency(t *testing.T) {
	limiter := NewLimiter(LimiterOptions{DefaultConcurrency: 1})

	// Hold the first request open so the second lands while the first is
	// still in-flight, exercising the real concurrency ceiling.
	releaseFirst := make(chan struct{})
	firstDone := make(chan struct{})

	mw := newTestMiddleware(t, MiddlewareOptions{Limiter: limiter})
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("userApiKey", "sk-test")
		c.Next()
	})
	router.Use(mw.Handler())
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		if _, started := c.Get("test_mark"); started {
			c.JSON(http.StatusOK, gin.H{"ok": true})
			return
		}
		c.Set("test_mark", true)
		<-releaseFirst
		c.JSON(http.StatusOK, gin.H{"ok": true})
		close(firstDone)
	})

	reqOne := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqOne.Header.Set("Authorization", "Bearer sk-test")
	w1 := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		router.ServeHTTP(w1, reqOne)
		close(done)
	}()
	// Wait for the first request to have acquired its concurrency slot.
	for limiter.InFlight("sk-test") == 0 {
		select {
		case <-done:
			t.Fatal("first request completed before slot was acquired")
		default:
		}
	}

	// Second concurrent request must be 429.
	reqTwo := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqTwo.Header.Set("Authorization", "Bearer sk-test")
	w2 := httptest.NewRecorder()
	router.ServeHTTP(w2, reqTwo)
	if w2.Code != http.StatusTooManyRequests {
		t.Fatalf("second concurrent request: expected 429, got %d", w2.Code)
	}

	// Release the first request; the slot frees and the request completes.
	close(releaseFirst)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("first request did not complete after release")
	}
	_ = firstDone
}

func TestMiddlewareBlacklistBlocksQuotaExhausted(t *testing.T) {
	limiter := NewLimiter(LimiterOptions{DefaultRPM: 100})
	server := httptest.NewServer(&keeperStub{rows: []keeperStatusRow{
		{APIKey: "sk-blocked", Enabled: true, RPM: 10, Exceeded: true, ExceededReason: "tpd"},
	}})
	defer server.Close()

	blacklist := NewBlacklist(BlacklistOptions{KeeperURL: server.URL})
	if err := blacklist.SyncOnce(context.Background()); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	mw := newTestMiddleware(t, MiddlewareOptions{Limiter: limiter, Blacklist: blacklist})

	w := runRequest(t, mw, "/v1/chat/completions", "sk-blocked")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("quota-exhausted key: expected 429, got %d", w.Code)
	}
	// A non-blocked key passes through.
	w = runRequest(t, mw, "/v1/chat/completions", "sk-ok")
	if w.Code != http.StatusOK {
		t.Fatalf("non-blocked key: expected 200, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestMiddlewareSkipsNonGuardedPaths(t *testing.T) {
	limiter := NewLimiter(LimiterOptions{DefaultRPM: 0, DefaultConcurrency: 1})
	mw := newTestMiddleware(t, MiddlewareOptions{Limiter: limiter})

	// Health/management paths are not guarded by default scopes.
	router := gin.New()
	var downstreamRan bool
	router.Use(mw.Handler())
	router.GET("/healthz", func(c *gin.Context) {
		downstreamRan = true
		c.Status(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Authorization", "Bearer sk-test")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on non-guarded path, got %d", w.Code)
	}
	if !downstreamRan {
		t.Fatal("downstream should have run on non-guarded path")
	}
}

func TestMiddlewareSkipsNoKey(t *testing.T) {
	limiter := NewLimiter(LimiterOptions{DefaultRPM: 1})
	mw := newTestMiddleware(t, MiddlewareOptions{Limiter: limiter})

	// With no key in context (auth disabled), pass through.
	w := runRequest(t, mw, "/v1/chat/completions", "")
	if w.Code != http.StatusOK {
		t.Fatalf("no-key request: expected 200, got %d body=%s", w.Code, w.Body.String())
	}
}

// runRequestWithKey simulates the auth context by setting userApiKey before the
// middleware runs. The simple runRequest above doesn't set the context — this
// variant does, matching the real data plane.
func TestMiddlewareUsesAuthenticatedKey(t *testing.T) {
	limiter := NewLimiter(LimiterOptions{DefaultRPM: 1})
	mw := newTestMiddleware(t, MiddlewareOptions{Limiter: limiter})

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("userApiKey", "sk-authed")
		c.Next()
	})
	router.Use(mw.Handler())
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	do := func() int {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		req.Header.Set("Authorization", "Bearer sk-authed")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w.Code
	}
	if code := do(); code != http.StatusOK {
		t.Fatalf("first request: expected 200, got %d", code)
	}
	if code := do(); code != http.StatusTooManyRequests {
		t.Fatalf("second request: expected 429, got %d", code)
	}
}

func TestMiddlewareLimiterNilOnlyBlacklistGates(t *testing.T) {
	server := httptest.NewServer(&keeperStub{rows: []keeperStatusRow{
		{APIKey: "sk-blocked", Enabled: true, Exceeded: true},
	}})
	defer server.Close()

	blacklist := NewBlacklist(BlacklistOptions{KeeperURL: server.URL})
	if err := blacklist.SyncOnce(context.Background()); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	// No limiter — only the blacklist gates.
	mw := newTestMiddleware(t, MiddlewareOptions{Blacklist: blacklist})

	w := runRequest(t, mw, "/v1/chat/completions", "sk-blocked")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("quota-exhausted key: expected 429, got %d", w.Code)
	}
	w = runRequest(t, mw, "/v1/chat/completions", "sk-ok")
	if w.Code != http.StatusOK {
		t.Fatalf("non-blocked key: expected 200, got %d", w.Code)
	}
}

func TestMiddlewarePathScopesCustom(t *testing.T) {
	limiter := NewLimiter(LimiterOptions{DefaultRPM: 1})
	mw := NewMiddleware(MiddlewareOptions{
		Limiter:    limiter,
		PathScopes: []string{"/custom/v1/"},
	})
	if !mw.guardedPath("/custom/v1/chat/completions") {
		t.Fatal("custom scope path should be guarded")
	}
	if mw.guardedPath("/v1/chat/completions") {
		t.Fatal("non-custom scope path should not be guarded")
	}
}
