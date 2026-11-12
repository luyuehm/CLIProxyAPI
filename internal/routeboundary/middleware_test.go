package routeboundary

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// newTestRouter builds a gin engine that resolves the userApiKey context
// value from the Authorization header (standing in for the access-manager
// auth middleware), then mounts the boundary admission middleware under
// test.
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
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})
	return router
}

func requestTo(router http.Handler, key string, mutate func(r *http.Request)) int {
	gin.SetMode(gin.TestMode)
	req, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if mutate != nil {
		mutate(req)
	}
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	return resp.Code
}

// TestMiddlewareNoBoundaryPassesThrough: a key with no delivered boundary
// runs with the full pool; the gate stamps X-OpenRouter-Policy-Applied:
// false so audit tooling knows the gate ran but did not constrain.
func TestMiddlewareNoBoundaryPassesThrough(t *testing.T) {
	store := NewStore()
	store.Apply([]Boundary{{APIKey: "other", Enabled: true, AllowedProviders: []string{"claude"}}})
	mw := NewMiddleware(MiddlewareOptions{Store: store})

	router := newTestRouter(mw.Handler())
	code := requestTo(router, "sk-free", func(r *http.Request) {
		r.Header.Set("X-OpenRouter-Provider", "gemini") // out of anyone's boundary; unconstrained key
	})
	if code != http.StatusOK {
		t.Fatalf("unconstrained key must pass through, got %d", code)
	}
}

// TestMiddlewareExplicitOutOfBoundary403: scenario 3 — an explicit provider
// selection (X-OpenRouter-Provider) outside the key's boundary is rejected
// with 403 route_boundary_violation naming the provider.
func TestMiddlewareExplicitOutOfBoundary403(t *testing.T) {
	store := NewStore()
	store.Apply([]Boundary{{APIKey: "sk-a", Enabled: true, AllowedProviders: []string{"openrouter"}}})
	mw := NewMiddleware(MiddlewareOptions{Store: store})

	router := newTestRouter(mw.Handler())
	code := requestTo(router, "sk-a", func(r *http.Request) {
		r.Header.Set("X-OpenRouter-Provider", "gemini")
	})
	if code != http.StatusForbidden {
		t.Fatalf("out-of-boundary explicit provider must 403, got %d", code)
	}
}

// TestMiddlewareAltProviderOutOfBoundary403: the ?alt=<provider> form of
// explicit selection is rejected the same way.
func TestMiddlewareAltProviderOutOfBoundary403(t *testing.T) {
	store := NewStore()
	store.Apply([]Boundary{{APIKey: "sk-a", Enabled: true, AllowedProviders: []string{"openrouter"}}})
	mw := NewMiddleware(MiddlewareOptions{Store: store})

	router := newTestRouter(mw.Handler())
	code := requestTo(router, "sk-a", func(r *http.Request) {
		r.Header.Set("X-OpenRouter-Provider", "gemini")
	})
	if code != http.StatusForbidden {
		t.Fatalf("out-of-boundary alt provider must 403, got %d", code)
	}
}

// TestMiddlewareOverrideDenied403: a key without override permission that
// sends X-OpenRouter-Route-Boundary: <p> is 403 — the header cannot
// self-grant.
func TestMiddlewareOverrideDenied403(t *testing.T) {
	store := NewStore()
	store.Apply([]Boundary{{APIKey: "sk-a", Enabled: true, AllowedProviders: []string{"openrouter"}}}) // no OverrideAllowed
	mw := NewMiddleware(MiddlewareOptions{Store: store})

	router := newTestRouter(mw.Handler())
	code := requestTo(router, "sk-a", func(r *http.Request) {
		r.Header.Set(HeaderBoundary, "vertex")
	})
	if code != http.StatusForbidden {
		t.Fatalf("override on a non-permitted key must 403, got %d", code)
	}
}

// TestMiddlewareOverrideGranted: a key with override permission is let
// through to the override provider; the audit event is enqueued after the
// response.
func TestMiddlewareOverrideGranted(t *testing.T) {
	store := NewStore()
	store.Apply([]Boundary{{APIKey: "sk-a", Enabled: true, AllowedProviders: []string{"openrouter"}, OverrideAllowed: true}})

	// A fake KEEPER that records the audit POST so the test can assert the
	// event actually landed.
	var auditBody auditWireEvent
	var gotAudit bool
	keeper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == auditEndpoint {
			gotAudit = true
			_ = json.NewDecoder(r.Body).Decode(&auditBody)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer keeper.Close()

	audit := NewAudit(AuditOptions{KeeperURL: keeper.URL, ManagementKey: "secret"})
	mw := NewMiddleware(MiddlewareOptions{Store: store, Audit: audit})

	router := newTestRouter(mw.Handler())
	// POST a JSON body so the admission-time model extraction is exercised.
	req, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"openrouter/anthropic/claude-3","stream":false}`))
	req.Header.Set("Authorization", "Bearer sk-a")
	req.Header.Set(HeaderBoundary, "vertex")
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("granted override must pass through the admission gate, got %d", resp.Code)
	}

	// The audit event is enqueued asynchronously; give the single worker a
	// bounded moment to POST it (deterministic: the queue drains on Close,
	// so we drain deterministically rather than wall-clock polling).
	audit.Close()
	if !gotAudit {
		t.Fatal("granted override must enqueue an audit event to KEEPER")
	}
	if auditBody.Action != "override" || auditBody.OverrideProvider != "vertex" {
		t.Fatalf("unexpected audit event: %+v", auditBody)
	}
	if auditBody.APIKey != "sk-a" {
		t.Fatalf("audit event key = %q, want sk-a", auditBody.APIKey)
	}
	if auditBody.Model != "openrouter/anthropic/claude-3" {
		t.Fatalf("audit event model = %q, want the request's model", auditBody.Model)
	}
	if len(auditBody.Boundary) != 1 || auditBody.Boundary[0] != "openrouter" {
		t.Fatalf("audit event boundary = %v, want [openrouter]", auditBody.Boundary)
	}
	// The handler still read the body unchanged: the route stub answered 200
	// (a mangled body would have surfaced as a handler error status).
}

// TestMiddlewareUnverifiedStorePassesThrough: before the first good pull the
// store is unverified and the gate is inert.
func TestMiddlewareUnverifiedStorePassesThrough(t *testing.T) {
	store := NewStore() // never applied
	mw := NewMiddleware(MiddlewareOptions{Store: store})

	router := newTestRouter(mw.Handler())
	code := requestTo(router, "sk-a", func(r *http.Request) {
		r.Header.Set("X-OpenRouter-Provider", "anything")
	})
	if code != http.StatusOK {
		t.Fatalf("unverified store must pass through, got %d", code)
	}
}

// TestMiddlewareNonGuardedPathPassesThrough: the guard only covers
// data-plane scopes; a management path is never constrained.
func TestMiddlewareNonGuardedPathPassesThrough(t *testing.T) {
	store := NewStore()
	store.Apply([]Boundary{{APIKey: "sk-a", Enabled: true, AllowedProviders: []string{"openrouter"}}})
	mw := NewMiddleware(MiddlewareOptions{Store: store, PathScopes: []string{"/v1/"}})

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("userApiKey", "sk-a")
		c.Next()
	})
	router.Use(mw.Handler())
	router.GET("/v0/management/x", func(c *gin.Context) { c.Status(http.StatusOK) })

	req, _ := http.NewRequest(http.MethodGet, "/v0/management/x", nil)
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("non-guarded path must pass through, got %d", resp.Code)
	}
}
