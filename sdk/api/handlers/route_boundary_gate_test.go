package handlers_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/routeboundary"
	handlers "github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

// rbGateExecutor is a stub ProviderExecutor that records which provider's
// executor the auth manager invoked, so a test can assert that no upstream
// request ever left the route boundary.
type rbGateExecutor struct {
	provider string

	mu    sync.Mutex
	calls int
}

func newRBGateExecutor(provider string) *rbGateExecutor { return &rbGateExecutor{provider: provider} }

func (e *rbGateExecutor) Identifier() string { return e.provider }

func (e *rbGateExecutor) CallCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

func (e *rbGateExecutor) Execute(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e *rbGateExecutor) ExecuteStream(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{Payload: []byte(`{"ok":true}`)}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *rbGateExecutor) Refresh(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *rbGateExecutor) CountTokens(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	return coreexecutor.Response{Payload: []byte(`{"total_tokens":1}`)}, nil
}

func (e *rbGateExecutor) HttpRequest(ctx context.Context, auth *coreauth.Auth, req *http.Request) (*http.Response, error) {
	return nil, &coreauth.Error{Code: "not_implemented", Message: "HttpRequest not implemented"}
}

// registerRBGateModel publishes a model under the given provider keys in the
// global registry and cleans up on test end. clientIDs[i] must equal the
// credential's auth.ID for providers[i]: the auth scheduler builds each
// credential's supported-model set from the registry keyed by the client ID
// at Register time (the proven newModelExecutionHandler pattern), so a
// mismatched client ID leaves the credential unselectable for the model.
func registerRBGateModel(t *testing.T, model string, clientIDs, providers []string) {
	t.Helper()
	for i, provider := range providers {
		registry.GetGlobalRegistry().RegisterClient(clientIDs[i], provider, []*registry.ModelInfo{{ID: model, Object: "model", OwnedBy: provider}})
		t.Cleanup(func() {
			registry.GetGlobalRegistry().UnregisterClient(clientIDs[i])
		})
	}
}

// newRBGateHandler builds a handler wired the way the production builder
// does: a live auth manager (KEEPER-managed objectstore credentials for the
// given providers), a route-boundary gate when boundary.Constrains(), and a
// recorder whose gin context the gate stamps. The fixture is deliberately
// minimal: no executors for providers that have no credential, so executor
// call counts pin the pool the executor actually saw.
func newRBGateHandler(t *testing.T, model string, providers []string, key string, boundary routeboundary.Boundary) (*handlers.BaseAPIHandler, map[string]*rbGateExecutor, *httptest.ResponseRecorder, *gin.Context) {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	executors := make(map[string]*rbGateExecutor, len(providers))
	clientIDs := make([]string, 0, len(providers))
	for _, provider := range providers {
		executor := newRBGateExecutor(provider)
		executors[provider] = executor
		manager.RegisterExecutor(executor)
		// objectstore source = a KEEPER-delivered (managed) credential, so
		// the enforcer's availability probe counts the provider usable.
		clientIDs = append(clientIDs, "rbgate-auth-"+model+"-"+provider)
		auth := &coreauth.Auth{
			ID:         clientIDs[len(clientIDs)-1],
			Provider:   provider,
			Status:     coreauth.StatusActive,
			Attributes: map[string]string{"source_backend": "objectstore", "api_key": "sk-managed-" + provider},
		}
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("manager.Register(%s): %v", provider, errRegister)
		}
	}
	registerRBGateModel(t, model, clientIDs, providers)

	handler := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	if boundary.Constrains() {
		store := routeboundary.NewStore()
		store.Apply([]routeboundary.Boundary{boundary})
		handler.SetRouteBoundaryGate(routeboundary.NewEnforcer(routeboundary.EnforcerOptions{
			Store: store,
			Probe: routeboundary.NewManagerProbe(manager),
		}))
	}

	// Build the execution context the way GetContextWithCancel does in
	// production: the gin context carries the caller key (set by the access
	// auth middleware) and the embedded request, and the gate stamps headers
	// on this context's writer.
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	request, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	request.Header.Set("Authorization", "Bearer "+key)
	ginCtx.Request = request
	ginCtx.Set("userApiKey", key)
	gin.SetMode(gin.TestMode)
	return handler, executors, recorder, ginCtx
}

// rbGateContext embeds the caller's gin context the way GetContextWithCancel
// does, so caller-key resolution and header stamping behave as in
// production.
func rbGateContext(ginCtx *gin.Context) context.Context {
	return context.WithValue(context.Background(), "gin", ginCtx)
}

// Scenario 1 (acceptance): the boundary holds and both allowed providers are
// usable → execution succeeds inside the boundary and stamps
// X-OpenRouter-Route-Boundary: allowed on the response.
func TestExecuteWithAuthManagerRouteBoundaryWithinBoundary(t *testing.T) {
	const model = "rbgate-model-inbound"
	const key = "sk-rb-inbound"
	handler, executors, recorder, ginCtx := newRBGateHandler(t, model, []string{"claude", "openrouter"}, key, routeboundary.Boundary{
		APIKey:           key,
		Enabled:          true,
		AllowedProviders: []string{"openrouter", "claude"},
	})

	ctx := rbGateContext(ginCtx)
	body, _, errMsg := handler.ExecuteWithAuthManager(ctx, "openai", model, []byte(`{"model":"`+model+`"}`), "")
	if errMsg != nil {
		t.Fatalf("in-boundary request violated: %+v", errMsg)
	}
	if len(body) == 0 {
		t.Fatal("expected an executor response body")
	}
	ran := false
	for _, executor := range executors {
		if executor.CallCount() > 0 {
			ran = true
		}
	}
	if !ran {
		t.Fatal("no executor was invoked; expected in-boundary execution")
	}
	if got := recorder.Header().Get(routeboundary.HeaderBoundary); got != "allowed" {
		t.Fatalf("X-OpenRouter-Route-Boundary = %q, want %q", got, "allowed")
	}
}

// Scenario 2 (acceptance): allowed list [claude, openrouter], but no usable
// managed credential inside it (only out-of-boundary "gemini" has one) →
// 503 route_boundary_violation and the gemini executor is never invoked.
func TestExecuteWithAuthManagerRouteBoundaryExhaustedPool(t *testing.T) {
	const model = "rbgate-model-exhausted"
	const key = "sk-rb-exhausted"

	// Only "gemini" gets a real credential + executor; the in-boundary
	// providers have none (no managed credential inside the boundary).
	manager := coreauth.NewManager(nil, nil, nil)
	geminiExecutor := newRBGateExecutor("gemini")
	manager.RegisterExecutor(geminiExecutor)
	geminiAuth := &coreauth.Auth{
		ID:         "rbgate-auth-" + model + "-gemini",
		Provider:   "gemini",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"source_backend": "objectstore", "api_key": "sk-managed-gemini"},
	}
	if _, errRegister := manager.Register(context.Background(), geminiAuth); errRegister != nil {
		t.Fatalf("manager.Register(gemini): %v", errRegister)
	}
	registerRBGateModel(t, model, []string{"rbgate-" + model + "-claude", geminiAuth.ID, "rbgate-" + model + "-openrouter"}, []string{"claude", "gemini", "openrouter"})

	store := routeboundary.NewStore()
	store.Apply([]routeboundary.Boundary{{
		APIKey:           key,
		Enabled:          true,
		AllowedProviders: []string{"claude", "openrouter"},
	}})
	handler := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	handler.SetRouteBoundaryGate(routeboundary.NewEnforcer(routeboundary.EnforcerOptions{
		Store: store,
		Probe: routeboundary.NewManagerProbe(manager),
	}))

	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	request, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ginCtx.Request = request
	ginCtx.Set("userApiKey", key)
	ctx := rbGateContext(ginCtx)
	_, _, errMsg := handler.ExecuteWithAuthManager(ctx, "openai", model, []byte(`{"model":"`+model+`"}`), "")
	if errMsg == nil {
		t.Fatal("exhausted boundary must fail, not degrade to gemini")
	}
	if errMsg.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", errMsg.StatusCode)
	}
	if !strings.Contains(string(errMsg.Body), `"code":"`+routeboundary.ErrorType+`"`) || !strings.Contains(string(errMsg.Body), model) {
		t.Fatalf("body missing violation code/model: %s", errMsg.Body)
	}
	if got := geminiExecutor.CallCount(); got != 0 {
		t.Fatalf("out-of-boundary gemini executor was invoked %d times; it must never be", got)
	}
}

// Scenario 3 (acceptance): boundary [claude]; an explicit provider selection
// for out-of-boundary "gemini" → 403 naming the provider.
func TestExecuteWithAuthManagerRouteBoundaryExplicitOutOfBoundary(t *testing.T) {
	const model = "rbgate-model-explicit"
	const key = "sk-rb-explicit"
	handler, executors, _, ginCtx := newRBGateHandler(t, model, []string{"claude", "gemini"}, key, routeboundary.Boundary{
		APIKey:           key,
		Enabled:          true,
		AllowedProviders: []string{"claude"},
	})

	// The gate reads the selection off the embedded gin request.
	ginCtx.Request.Header.Set("X-OpenRouter-Provider", "gemini")
	ctx := rbGateContext(ginCtx)

	_, _, errMsg := handler.ExecuteWithAuthManager(ctx, "openai", model, []byte(`{"model":"`+model+`"}`), "")
	if errMsg == nil {
		t.Fatal("out-of-boundary explicit provider must be rejected")
	}
	if errMsg.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", errMsg.StatusCode)
	}
	if !strings.Contains(string(errMsg.Body), "gemini is not within the configured route boundary") {
		t.Fatalf("body must name the provider: %s", errMsg.Body)
	}
	for provider, executor := range executors {
		if executor.CallCount() != 0 {
			t.Fatalf("provider %s executor was invoked; the request must be rejected before execution", provider)
		}
	}
}

// Scenario 4 (acceptance): overrideAllowed=true + an admission-granted
// override pins execution to the out-of-boundary provider and stamps the
// override header.
func TestExecuteWithAuthManagerRouteBoundaryOverrideGranted(t *testing.T) {
	const model = "rbgate-model-override"
	const key = "sk-rb-override"
	handler, executors, recorder, ginCtx := newRBGateHandler(t, model, []string{"claude", "gemini"}, key, routeboundary.Boundary{
		APIKey:           key,
		Enabled:          true,
		AllowedProviders: []string{"claude"},
		OverrideAllowed:  true,
	})

	// The admission middleware would have granted + audited the override;
	// the gate only pins the pool on the granted marker.
	routeboundary.GrantOverride(ginCtx, "gemini")
	ctx := rbGateContext(ginCtx)

	body, _, errMsg := handler.ExecuteWithAuthManager(ctx, "openai", model, []byte(`{"model":"`+model+`"}`), "")
	if errMsg != nil {
		t.Fatalf("granted override violated: %+v", errMsg)
	}
	if len(body) == 0 {
		t.Fatal("expected an executor response body")
	}
	if got := executors["gemini"].CallCount(); got != 1 {
		t.Fatalf("override must pin execution to gemini, got %d calls", got)
	}
	if got := executors["claude"].CallCount(); got != 0 {
		t.Fatalf("claude must not be invoked on a gemini override, got %d calls", got)
	}
	if got := recorder.Header().Get(routeboundary.HeaderBoundary); got != "override" {
		t.Fatalf("X-OpenRouter-Route-Boundary = %q, want %q", got, "override")
	}
}

// Call site 2: the count path narrows through the gate too. With
// strict=true and the preferred provider usable, the pool collapses to the
// preferred provider and never falls to the rest of the list.
func TestExecuteCountWithAuthManagerRouteBoundaryStrict(t *testing.T) {
	const model = "rbgate-model-count"
	const key = "sk-rb-count"
	handler, executors, _, ginCtx := newRBGateHandler(t, model, []string{"claude", "gemini"}, key, routeboundary.Boundary{
		APIKey:           key,
		Enabled:          true,
		AllowedProviders: []string{"claude", "gemini"},
		Strict:           true,
	})

	ctx := rbGateContext(ginCtx)
	_, _, errMsg := handler.ExecuteCountWithAuthManager(ctx, "openai", model, []byte(`{"model":"`+model+`"}`), "")
	if errMsg != nil {
		t.Fatalf("strict pin to a usable preferred provider must succeed: %+v", errMsg)
	}
	if got := executors["claude"].CallCount(); got != 1 {
		t.Fatalf("strict must pin the preferred provider, claude calls = %d", got)
	}
	if got := executors["gemini"].CallCount(); got != 0 {
		t.Fatalf("strict must not fall to the rest of the list, gemini calls = %d", got)
	}
}

// Call site 3: the streaming path narrows through the gate too; an in-
// boundary stream delivers chunks and stamps the allowed header.
func TestExecuteStreamWithAuthManagerRouteBoundaryWithinBoundary(t *testing.T) {
	const model = "rbgate-model-stream"
	const key = "sk-rb-stream"
	handler, executors, recorder, ginCtx := newRBGateHandler(t, model, []string{"claude", "gemini"}, key, routeboundary.Boundary{
		APIKey:           key,
		Enabled:          true,
		AllowedProviders: []string{"claude"},
	})

	ctx := rbGateContext(ginCtx)
	dataChan, _, errChan := handler.ExecuteStreamWithAuthManager(ctx, "openai", model, []byte(`{"model":"`+model+`"}`), "")
	select {
	case chunk := <-dataChan:
		if len(chunk) == 0 {
			t.Fatal("expected a non-empty stream chunk")
		}
	case errMsg, ok := <-errChan:
		if ok && errMsg != nil {
			t.Fatalf("stream violated: %+v", errMsg)
		}
		t.Fatal("stream ended without data")
	}
	if got := recorder.Header().Get(routeboundary.HeaderBoundary); got != "allowed" {
		t.Fatalf("X-OpenRouter-Route-Boundary = %q, want %q", got, "allowed")
	}
	for provider, executor := range executors {
		if executor.CallCount() > 0 && provider != "claude" {
			t.Fatalf("executed provider %s is outside the boundary", provider)
		}
	}
}

// Regression guard: a handler with no gate (the default build) routes with
// the full pool and stamps no boundary headers.
func TestExecuteWithAuthManagerNoGateRoutesUnchanged(t *testing.T) {
	const model = "rbgate-model-nogate"
	const key = "sk-rb-nogate"
	handler, _, recorder, ginCtx := newRBGateHandler(t, model, []string{"claude", "gemini"}, key, routeboundary.Boundary{})

	ctx := rbGateContext(ginCtx)
	body, _, errMsg := handler.ExecuteWithAuthManager(ctx, "openai", model, []byte(`{"model":"`+model+`"}`), "")
	if errMsg != nil {
		t.Fatalf("no gate must not constrain routing: %+v", errMsg)
	}
	if len(body) == 0 {
		t.Fatal("expected an executor response body")
	}
	if got := recorder.Header().Get(routeboundary.HeaderBoundary); got != "" {
		t.Fatalf("default build must stamp no boundary header, got %q", got)
	}
}
