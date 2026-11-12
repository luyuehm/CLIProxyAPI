package routeboundary

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// testProbe is a hand-built AvailabilityProbe for gate tests: it reports
// managed-availability exactly as its tables say, with no live auth manager.
type testProbe struct {
	managed map[string]bool
}

func (p *testProbe) HasManagedCredential(provider string) bool {
	if p == nil || p.managed == nil {
		return true
	}
	key := normalizeProvider(provider)
	if _, ok := p.managed[key]; ok {
		return p.managed[key]
	}
	return false // unknown provider: no managed credential
}

// newGateContext builds an execution context the way the handlers embed the
// gin context: a request carrying the caller's bearer key, optionally with a
// resolved userApiKey principal.
func newGateContext(t *testing.T, key string, mutate func(r *http.Request)) context.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	req, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if mutate != nil {
		mutate(req)
	}
	c.Request = req
	if key != "" {
		c.Set("userApiKey", key)
	}
	return context.WithValue(context.Background(), "gin", c)
}

// TestApplyBoundaryPassThroughWhenNoBoundary: a key with no delivered
// boundary runs with the full pool, no violation.
func TestApplyBoundaryPassThroughWhenNoBoundary(t *testing.T) {
	store := NewStore()
	store.Apply([]Boundary{
		{APIKey: "other", Enabled: true, AllowedProviders: []string{"claude"}},
	})
	probe := &testProbe{managed: map[string]bool{"openrouter": true, "claude": true}}
	enf := NewEnforcer(EnforcerOptions{Store: store, Probe: probe})

	ctx := newGateContext(t, "sk-free", nil)
	pool, errMsg := enf.ApplyBoundary(ctx, []string{"openrouter", "claude", "gemini"}, "claude-3", "", "")
	if errMsg != nil {
		t.Fatalf("pass-through produced violation: %+v", errMsg)
	}
	if len(pool) != 3 {
		t.Fatalf("unconstrained key must keep the full pool, got %v", pool)
	}
}

// TestApplyBoundaryUnverifiedStorePassesThrough: before the first good pull
// the gate is inert.
func TestApplyBoundaryUnverifiedStorePassesThrough(t *testing.T) {
	store := NewStore() // never applied
	probe := &testProbe{managed: map[string]bool{"claude": true}}
	enf := NewEnforcer(EnforcerOptions{Store: store, Probe: probe})

	ctx := newGateContext(t, "sk-a", nil)
	pool, errMsg := enf.ApplyBoundary(ctx, []string{"claude", "gemini"}, "claude-3", "", "")
	if errMsg != nil || len(pool) != 2 {
		t.Fatalf("unverified store must pass through: pool=%v err=%+v", pool, errMsg)
	}
}

// TestApplyBoundaryNarrowsPool: scenario 1. Boundary [openrouter, claude]
// narrows a 3-provider pool to the two allowed, in pool preference order.
func TestApplyBoundaryNarrowsPool(t *testing.T) {
	store := NewStore()
	store.Apply([]Boundary{
		{APIKey: "sk-a", Enabled: true, AllowedProviders: []string{"claude", "openrouter"}},
	})
	probe := &testProbe{managed: map[string]bool{"openrouter": true, "claude": true, "gemini": true}}
	enf := NewEnforcer(EnforcerOptions{Store: store, Probe: probe})

	ctx := newGateContext(t, "sk-a", nil)
	pool, errMsg := enf.ApplyBoundary(ctx, []string{"gemini", "openrouter", "claude"}, "claude-3", "", "")
	if errMsg != nil {
		t.Fatalf("in-boundary request violated: %+v", errMsg)
	}
	want := []string{"openrouter", "claude"}
	if len(pool) != len(want) {
		t.Fatalf("pool = %v, want %v", pool, want)
	}
	for i := range want {
		if pool[i] != want[i] {
			t.Fatalf("pool = %v, want %v (preference order kept)", pool, want)
		}
	}
}

// TestApplyBoundaryExhaustedPool503: scenario 2. The allowed providers have
// no usable managed credential → 503 route_boundary_violation, no pool.
func TestApplyBoundaryExhaustedPool503(t *testing.T) {
	store := NewStore()
	store.Apply([]Boundary{
		{APIKey: "sk-a", Enabled: true, AllowedProviders: []string{"openrouter", "claude"}},
	})
	// Both allowed providers lack managed credentials.
	probe := &testProbe{managed: map[string]bool{"openrouter": false, "claude": false}}
	enf := NewEnforcer(EnforcerOptions{Store: store, Probe: probe})

	ctx := newGateContext(t, "sk-a", nil)
	pool, errMsg := enf.ApplyBoundary(ctx, []string{"openrouter", "claude", "gemini"}, "claude-3", "", "")
	if errMsg == nil {
		t.Fatal("exhausted boundary must fail, not degrade to gemini")
	}
	if pool != nil {
		t.Fatalf("violation must not yield a pool, got %v", pool)
	}
	if errMsg.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", errMsg.StatusCode)
	}
	if !strings.Contains(string(errMsg.Body), `"code":"`+ErrorType+`"`) {
		t.Fatalf("body missing %s: %s", ErrorType, errMsg.Body)
	}
	if !strings.Contains(string(errMsg.Body), "claude-3") {
		t.Fatalf("body must name the model: %s", errMsg.Body)
	}
}

// TestApplyBoundaryExplicitOutOfBoundary403: scenario 3. alt names a provider
// outside the boundary → 403, naming that provider.
func TestApplyBoundaryExplicitOutOfBoundary403(t *testing.T) {
	store := NewStore()
	store.Apply([]Boundary{
		{APIKey: "sk-a", Enabled: true, AllowedProviders: []string{"openrouter"}},
	})
	probe := &testProbe{managed: map[string]bool{"openrouter": true}}
	enf := NewEnforcer(EnforcerOptions{Store: store, Probe: probe})

	ctx := newGateContext(t, "sk-a", func(r *http.Request) {
		r.Header.Set(HeaderExplicitProvider, "gemini")
	})
	pool, errMsg := enf.ApplyBoundary(ctx, []string{"openrouter", "gemini"}, "claude-3", "", "")
	if errMsg == nil {
		t.Fatal("explicit out-of-boundary provider must be rejected")
	}
	if pool != nil {
		t.Fatalf("rejection must not yield a pool, got %v", pool)
	}
	if errMsg.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", errMsg.StatusCode)
	}
	if !strings.Contains(string(errMsg.Body), "gemini is not within the configured route boundary") {
		t.Fatalf("body must name the provider: %s", errMsg.Body)
	}
}

// TestApplyBoundaryForcedProviderOutOfBoundary403: the internal forced
// provider (executor option) is checked against the boundary too.
func TestApplyBoundaryForcedProviderOutOfBoundary403(t *testing.T) {
	store := NewStore()
	store.Apply([]Boundary{
		{APIKey: "sk-a", Enabled: true, AllowedProviders: []string{"openrouter"}},
	})
	probe := &testProbe{managed: map[string]bool{"openrouter": true}}
	enf := NewEnforcer(EnforcerOptions{Store: store, Probe: probe})

	ctx := newGateContext(t, "sk-a", nil)
	pool, errMsg := enf.ApplyBoundary(ctx, []string{"openrouter"}, "claude-3", "gemini", "")
	if errMsg == nil || errMsg.StatusCode != http.StatusForbidden {
		t.Fatalf("forced provider out of boundary must 403: pool=%v err=%+v", pool, errMsg)
	}
}

// TestApplyBoundaryStrictPinsPreferred: strict=true pins to the top-priority
// allowed provider when it is usable; the rest of the list is not walked.
func TestApplyBoundaryStrictPinsPreferred(t *testing.T) {
	store := NewStore()
	store.Apply([]Boundary{
		{APIKey: "sk-a", Enabled: true, AllowedProviders: []string{"claude", "openrouter"}, Strict: true},
	})
	probe := &testProbe{managed: map[string]bool{"claude": true, "openrouter": true}}
	enf := NewEnforcer(EnforcerOptions{Store: store, Probe: probe})

	ctx := newGateContext(t, "sk-a", nil)
	pool, errMsg := enf.ApplyBoundary(ctx, []string{"openrouter", "claude"}, "claude-3", "", "")
	if errMsg != nil {
		t.Fatalf("strict with usable preferred must succeed: %+v", errMsg)
	}
	if len(pool) != 1 || pool[0] != "claude" {
		t.Fatalf("strict must pin the preferred provider, got %v", pool)
	}
}

// TestApplyBoundaryStrictPreferredUnavailable503: strict=true with the
// preferred provider unusable fails immediately (no fallback down the list).
func TestApplyBoundaryStrictPreferredUnavailable503(t *testing.T) {
	store := NewStore()
	store.Apply([]Boundary{
		{APIKey: "sk-a", Enabled: true, AllowedProviders: []string{"claude", "openrouter"}, Strict: true},
	})
	// Preferred "claude" is unusable but the second ("openrouter") would be.
	probe := &testProbe{managed: map[string]bool{"claude": false, "openrouter": true}}
	enf := NewEnforcer(EnforcerOptions{Store: store, Probe: probe})

	ctx := newGateContext(t, "sk-a", nil)
	pool, errMsg := enf.ApplyBoundary(ctx, []string{"openrouter", "claude"}, "claude-3", "", "")
	if errMsg == nil {
		t.Fatal("strict must fail when the preferred provider is unusable")
	}
	if pool != nil || errMsg.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("strict miss = 503 with no pool, got pool=%v status=%d", pool, errMsg.StatusCode)
	}
}

// TestApplyBoundaryOverrideGranted: overrideAllowed=true + the admission
// marker pin the execution to the out-of-boundary provider.
func TestApplyBoundaryOverrideGranted(t *testing.T) {
	store := NewStore()
	store.Apply([]Boundary{
		{APIKey: "sk-a", Enabled: true, AllowedProviders: []string{"claude"}, OverrideAllowed: true},
	})
	// The override provider has no managed credential at all — the point of
	// the override is to reach an out-of-boundary target.
	probe := &testProbe{managed: map[string]bool{"claude": true}}
	enf := NewEnforcer(EnforcerOptions{Store: store, Probe: probe})

	ctx := newGateContext(t, "sk-a", nil)
	// The admission middleware would have set this after granting.
	if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok {
		ginCtx.Set(overrideCtxKey, "vertex")
	}
	pool, errMsg := enf.ApplyBoundary(ctx, []string{"claude"}, "claude-3", "", "")
	if errMsg != nil {
		t.Fatalf("granted override must not violate: %+v", errMsg)
	}
	if len(pool) != 1 || pool[0] != "vertex" {
		t.Fatalf("override must pin the granted provider, got %v", pool)
	}
}

// TestApplyBoundaryOverrideHeaderNotGranted: a raw X-OpenRouter-Route-Boundary
// header is only honoured through the admission-granted marker — a caller
// cannot self-grant an override. Here the key lacks override permission, the
// middleware would have 403'd the header (so no marker is set), and the gate
// must therefore treat the request as a plain boundary request: the pool is
// narrowed to the boundary, dropping the out-of-boundary "vertex".
func TestApplyBoundaryOverrideHeaderNotGranted(t *testing.T) {
	store := NewStore()
	store.Apply([]Boundary{
		{APIKey: "sk-a", Enabled: true, AllowedProviders: []string{"claude"}}, // no override permission
	})
	probe := &testProbe{managed: map[string]bool{"claude": true}}
	enf := NewEnforcer(EnforcerOptions{Store: store, Probe: probe})

	ctx := newGateContext(t, "sk-a", func(r *http.Request) {
		r.Header.Set(HeaderBoundary, "vertex") // present but NOT granted
	})
	pool, errMsg := enf.ApplyBoundary(ctx, []string{"claude", "vertex"}, "claude-3", "", "")
	if errMsg != nil {
		t.Fatalf("plain in-boundary request must not violate: %+v", errMsg)
	}
	// The raw header must not pin the pool to "vertex"; only the boundary
	// provider survives.
	if len(pool) != 1 || pool[0] != "claude" {
		t.Fatalf("raw override header must not self-grant; pool = %v, want [claude]", pool)
	}
}

// TestNilEnforcerPassesThrough: a nil gate (channel off / not installed)
// leaves everything untouched — the default build.
func TestNilEnforcerPassesThrough(t *testing.T) {
	var enf *Enforcer
	pool, errMsg := enf.ApplyBoundary(context.Background(), []string{"a", "b"}, "m", "x", "y")
	if errMsg != nil {
		t.Fatalf("nil enforcer must not violate: %+v", errMsg)
	}
	if len(pool) != 2 {
		t.Fatalf("nil enforcer must pass the pool through, got %v", pool)
	}
}

// TestTranslateExecutionErrorConstrainedCooldown covers the post-execution
// half of scenario 2: a boundary-constrained request whose pool's only
// usable credential ends up cooling down mid-flight surfaces as the
// contract's 503 route_boundary_violation, not the raw cooldown error.
func TestTranslateExecutionErrorConstrainedCooldown(t *testing.T) {
	store := NewStore()
	store.Apply([]Boundary{{APIKey: "sk-a", Enabled: true, AllowedProviders: []string{"claude"}}})
	probe := &testProbe{managed: map[string]bool{"claude": true}}
	enf := NewEnforcer(EnforcerOptions{Store: store, Probe: probe})

	ctx := newGateContext(t, "sk-a", nil)
	_, errMsg := enf.ApplyBoundary(ctx, []string{"claude", "gemini"}, "claude-3", "", "")
	if errMsg != nil {
		t.Fatalf("admission must succeed for a usable pool: %+v", errMsg)
	}
	// The model-level cooldown error is unexported; the handler surfaces it
	// through IsModelCooldown(), which the enforcer detects via the same
	// marker interface.
	cooldown := modelCooldownMarkerError{}
	translated := enf.TranslateExecutionError(ctx, cooldown, "claude-3")
	if translated == nil {
		t.Fatal("a constrained request's cooldown failure must translate to a violation")
	}
	if translated.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("translated status = %d, want 503", translated.StatusCode)
	}
	if !strings.Contains(string(translated.Body), ErrorType) {
		t.Fatalf("translated body missing %s: %s", ErrorType, translated.Body)
	}
	// An unconstrained request keeps its raw error.
	ctx2 := newGateContext(t, "sk-free", nil)
	if got := enf.TranslateExecutionError(ctx2, cooldown, "claude-3"); got != nil {
		t.Fatalf("unconstrained request must keep its raw error, got %+v", got)
	}
	// A non-availability failure on a constrained request also keeps its
	// raw error.
	if got := enf.TranslateExecutionError(ctx, errors.New("upstream 500"), "claude-3"); got != nil {
		t.Fatalf("non-availability failure must not translate, got %+v", got)
	}
}

// modelCooldownMarkerError is a test double for the auth scheduler's
// unexported model-cooldown error, detected through the same
// IsModelCooldown() marker interface the production code uses.
type modelCooldownMarkerError struct{}

func (modelCooldownMarkerError) IsModelCooldown() bool { return true }
func (modelCooldownMarkerError) Error() string         { return "All credentials for model are cooling down" }

// TestManagerProbeClassifiesManaged covers the BYOK rabbit hole with a real
// auth manager: a boundary whose only in-boundary credential is a local
// BYOK file is rejected; a KEEPER-delivered (objectstore) credential is
// accepted.
func TestManagerProbeClassifiesManaged(t *testing.T) {
	// BYOK credential: a local file source.
	manager := coreauth.NewManager(nil, nil, nil)
	byok := &coreauth.Auth{
		ID:       "byok-1",
		Provider: "claude",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"source": "file:auths/byok.json",
		},
	}
	if _, err := manager.Register(context.Background(), byok); err != nil {
		t.Fatalf("register byok: %v", err)
	}

	probe := NewManagerProbe(manager)
	if probe.HasManagedCredential("claude") {
		t.Fatal("a BYOK-only provider must not count as managed")
	}
	if !manager.HasProviderAuth("claude") {
		t.Fatal("a BYOK provider does have a credential")
	}

	// Add a KEEPER-delivered (objectstore) credential for a second provider.
	delivered := &coreauth.Auth{
		ID:       "keeper-1",
		Provider: "openrouter",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"source_backend": "objectstore",
			"api_key":        "sk-managed",
		},
	}
	if _, err := manager.Register(context.Background(), delivered); err != nil {
		t.Fatalf("register delivered: %v", err)
	}
	if !probe.HasManagedCredential("openrouter") {
		t.Fatal("a KEEPER-delivered credential must count as managed")
	}

	// The BYOK provider is now still not managed on its own.
	if probe.HasManagedCredential("claude") {
		t.Fatal("adding a managed credential for another provider must not make claude managed")
	}
}
