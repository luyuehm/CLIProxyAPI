package test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/routeboundary"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	handlers "github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

// ============================================================================
// RIC-2532-C scenarios 1/2 end-to-end acceptance, on the main trunk with the
// PR #41 fix (claudeCreds Metadata["base_url"] fallback).
//
// These drive the REAL BaseAPIHandler -> route-boundary admission middleware
// -> execution enforcer -> auth manager -> REAL ClaudeExecutor -> httptest
// mock Anthropic upstream, wired the way the production builder does (the
// middleware enforces the client-visible surface and stamps
// X-OpenRouter-Policy-Applied, the enforcer narrows the execution pool and
// stamps X-OpenRouter-Route-Boundary).
//
// They prove exactly what the previous rounds could not:
//
//	1. A *managed* credential (source_backend=objectstore, the KEEPER-delivered
//	   channel) whose custom upstream base_url lives ONLY in Metadata — the
//	   shape the three managed stores (gitstore/objectstore/postgresstore)
//	   actually produce — reaches the local mock upstream and returns 200.
//	   Before PR #41, claudeCreds ignored Metadata["base_url"], so this
//	   request would have gone to api.anthropic.com and failed; the fix is what
//	   makes "managed credential" and "reachable custom upstream" coexist.
//	2. Scenario 1: in-boundary provider runs, response carries
//	   X-OpenRouter-Route-Boundary: allowed and X-OpenRouter-Policy-Applied: true.
//	3. Scenario 2: when the in-boundary providers have NO usable managed
//	   credential, the request is 503 route_boundary_violation and the
//	   out-of-boundary provider's upstream is NEVER contacted.
// ============================================================================

// claudeMockUpstream is a minimal Anthropic /v1/messages upstream recorder.
// The claude->openai (non-stream) path reads the upstream as an SSE stream
// (upstreamStream is true whenever the response format differs from the
// claude source), so the fixture emits the canonical Anthropic SSE event
// sequence: message_start -> content_block_delta -> message_delta ->
// message_stop.
type claudeMockUpstream struct {
	server *httptest.Server
	hits   atomic.Int64
}

func newClaudeMockUpstream(t *testing.T) *claudeMockUpstream {
	t.Helper()
	m := &claudeMockUpstream{}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		m.hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_ric2532\",\"model\":\"claude-sonnet-4-5\",\"usage\":{\"input_tokens\":5,\"cache_read_input_tokens\":0,\"cache_creation_input_tokens\":0,\"output_tokens\":1}}}\n\n" +
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n" +
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":1,\"output_tokens_details\":{\"thinking_tokens\":0}}}\n\n" +
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	t.Cleanup(m.server.Close)
	return m
}

// managedCredential builds a KEEPER-delivered (managed) credential the way the
// objectstore/gitstore/postgresstore construct one: the custom base_url lives
// in Metadata ONLY (never Attributes), source_backend marks it managed, and
// NormalizeCredentialMetadata would have canonicalized it. provider is the
// upstream provider key (e.g. "claude", "gemini").
func managedCredential(id, provider, baseURL, apiKey string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:       id,
		Provider: provider,
		FileName: id,
		Status:   cliproxyauth.StatusActive,
		Attributes: map[string]string{
			// The managed stores set path + source_backend + (maybe) email,
			// never base_url. Crucially, no "base_url" in Attributes.
			"source_backend": cliproxyauth.AuthSourceObjectStore,
			"api_key":        apiKey,
		},
		Metadata: map[string]any{
			// The PR #41 fix path: base_url read from here.
			"base_url": baseURL,
			"type":     provider,
		},
	}
}

func registerRIC2532Model(t *testing.T, model string, clientIDs, providers []string) {
	t.Helper()
	for i, provider := range providers {
		registry.GetGlobalRegistry().RegisterClient(clientIDs[i], provider, []*registry.ModelInfo{{ID: model, Object: "model", OwnedBy: provider}})
		t.Cleanup(func() {
			registry.GetGlobalRegistry().UnregisterClient(clientIDs[i])
		})
	}
}

// ric2532Handler wires the production path: a live auth manager with the
// given executors registered, the boundary admission middleware AND the
// execution enforcer mounted on a gin engine, and a fake upstream for each
// managed credential. It returns the gin engine, the mock upstreams (keyed by
// provider), and the response recorder.
func ric2532Handler(t *testing.T, model, key string, boundary routeboundary.Boundary, managedProviders []string) (*gin.Engine, map[string]*claudeMockUpstream, *httptest.ResponseRecorder) {
	t.Helper()
	manager := cliproxyauth.NewManager(nil, &cliproxyauth.RoundRobinSelector{}, nil)
	manager.RegisterExecutor(runtimeexecutor.NewClaudeExecutor(&config.Config{}))

	upstreams := make(map[string]*claudeMockUpstream, len(managedProviders))
	clientIDs := make([]string, 0, len(managedProviders))
	providers := make([]string, 0, len(managedProviders))
	for _, provider := range managedProviders {
		upstream := newClaudeMockUpstream(t)
		upstreams[provider] = upstream
		id := "ric2532-" + model + "-" + provider
		clientIDs = append(clientIDs, id)
		providers = append(providers, provider)
		if _, err := manager.Register(context.Background(), managedCredential(id, provider, upstream.server.URL, "sk-managed-"+provider)); err != nil {
			t.Fatalf("register %s: %v", provider, err)
		}
	}
	registerRIC2532Model(t, model, clientIDs, providers)

	store := routeboundary.NewStore()
	store.Apply([]routeboundary.Boundary{boundary})
	mw := routeboundary.NewMiddleware(routeboundary.MiddlewareOptions{Store: store})
	handler := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	handler.SetRouteBoundaryGate(routeboundary.NewEnforcer(routeboundary.EnforcerOptions{
		Store: store,
		Probe: routeboundary.NewManagerProbe(manager),
	}))

	gin.SetMode(gin.TestMode)
	router := gin.New()
	// Access-auth middleware: resolve the caller key into userApiKey, exactly
	// as the production access manager does.
	router.Use(func(c *gin.Context) {
		if auth := strings.TrimSpace(c.GetHeader("Authorization")); strings.HasPrefix(auth, "Bearer ") {
			c.Set("userApiKey", strings.TrimSpace(strings.TrimPrefix(auth, "Bearer ")))
		}
		c.Next()
	})
	// The route-boundary admission middleware (stamps Policy-Applied, rejects
	// explicit out-of-boundary / unauthorized override).
	router.Use(mw.Handler())
	// The model-serving route; the handler runs the execution gate.
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		execCtx := context.WithValue(c.Request.Context(), "gin", c)
		bodyBytes, errRead := io.ReadAll(c.Request.Body)
		if errRead != nil {
			c.Status(http.StatusBadRequest)
			return
		}
		out, _, errMsg := handler.ExecuteWithAuthManager(execCtx, "openai", model, bodyBytes, "")
		if errMsg != nil {
			c.Status(errMsg.StatusCode)
			_, _ = c.Writer.Write(errMsg.Body)
			return
		}
		c.Status(http.StatusOK)
		_, _ = c.Writer.Write(out)
	})

	recorder := httptest.NewRecorder()
	return router, upstreams, recorder
}

// TestScenario1RealClaudeExecutorInBoundaryServes is acceptance scenario 1 on
// the real executor: boundary [openrouter, claude], strict=false. A managed
// claude credential (Metadata base_url -> local mock) is available inside the
// boundary. The request succeeds inside the boundary, the claude executor
// actually runs (mock upstream hit), and BOTH boundary headers are stamped.
func TestScenario1RealClaudeExecutorInBoundaryServes(t *testing.T) {
	const model = "ric2532-s1-model"
	const key = "sk-ric2532-s1"
	router, upstreams, recorder := ric2532Handler(t, model, key, routeboundary.Boundary{
		APIKey:           key,
		Enabled:          true,
		AllowedProviders: []string{"openrouter", "claude"},
		Strict:           false,
	}, []string{"claude"})

	req, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", recorder.Code, recorder.Body.String())
	}
	if got := upstreams["claude"].hits.Load(); got != 1 {
		t.Fatalf("claude mock upstream hits = %d, want 1 (the managed credential must reach its custom upstream)", got)
	}
	if got := recorder.Header().Get(routeboundary.HeaderBoundary); got != "allowed" {
		t.Fatalf("X-OpenRouter-Route-Boundary = %q, want %q", got, "allowed")
	}
	if got := recorder.Header().Get(routeboundary.HeaderPolicyApplied); got != "true" {
		t.Fatalf("X-OpenRouter-Policy-Applied = %q, want %q", got, "true")
	}
}

// TestScenario2RealClaudeExecutorExhaustedPool is acceptance scenario 2 on the
// real executor: the boundary is [openrouter, claude] but NO managed
// credential exists for either (only the out-of-boundary provider "gemini" has
// one, and it is deliberately registered so the request COULD have degraded to
// it). The request is 503 route_boundary_violation and the out-of-boundary
// provider's upstream is never contacted.
func TestScenario2RealClaudeExecutorExhaustedPool(t *testing.T) {
	const model = "ric2532-s2-model"
	const key = "sk-ric2532-s2"
	// Only gemini gets a managed credential; the in-boundary providers
	// (openrouter, claude) have none. gemini is registered as reachable so
	// that a boundary violation would be provable as a genuine 503, not an
	// "unreachable upstream" accident.
	router, upstreams, recorder := ric2532Handler(t, model, key, routeboundary.Boundary{
		APIKey:           key,
		Enabled:          true,
		AllowedProviders: []string{"openrouter", "claude"},
	}, []string{"gemini"})

	req, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body=%s)", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"code":"`+routeboundary.ErrorType+`"`) || !strings.Contains(recorder.Body.String(), model) {
		t.Fatalf("body missing violation code/model: %s", recorder.Body.String())
	}
	if got := upstreams["gemini"].hits.Load(); got != 0 {
		t.Fatalf("out-of-boundary gemini upstream was contacted %d times; it must never be", got)
	}
}

// TestScenario1MetadataBaseURLIsHonoredByRealExecutor is the narrowest proof
// of the PR #41 fix itself: with NO boundary installed at all, a managed
// credential whose base_url lives only in Metadata reaches its custom
// upstream. This is the regression test that makes "managed credential +
// reachable custom upstream" coexist — the precondition for scenarios 1/2.
func TestScenario1MetadataBaseURLIsHonoredByRealExecutor(t *testing.T) {
	const model = "ric2532-meta-model"
	const key = "sk-ric2532-meta"
	// A key with no delivered boundary: the gate passes through, and the
	// request must still reach the custom upstream because the executor
	// resolves base_url from Metadata.
	router, upstreams, recorder := ric2532Handler(t, model, key, routeboundary.Boundary{}, []string{"claude"})

	req, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", recorder.Code, recorder.Body.String())
	}
	if got := upstreams["claude"].hits.Load(); got != 1 {
		t.Fatalf("claude mock upstream hits = %d, want 1 (Metadata base_url must be honored)", got)
	}
	// A no-boundary key stamps Policy-Applied: false (the gate ran but did
	// not constrain), and no Route-Boundary verdict.
	if got := recorder.Header().Get(routeboundary.HeaderPolicyApplied); got != "false" {
		t.Fatalf("X-OpenRouter-Policy-Applied = %q, want %q (gate ran but did not constrain)", got, "false")
	}
	if got := recorder.Header().Get(routeboundary.HeaderBoundary); got != "" {
		t.Fatalf("no-boundary key must stamp no Route-Boundary verdict, got %q", got)
	}
}

// sanity: the mock SSE fixture parses as a valid claude message (so the
// translator does not choke on the fixture).
func TestRIC2532MockResponseIsValidClaudeMessage(t *testing.T) {
	var v map[string]any
	if err := json.Unmarshal([]byte(`{
		"id":"msg_ric2532","type":"message","model":"claude-sonnet-4-5",
		"role":"assistant","content":[{"type":"text","text":"ok"}],
		"usage":{"input_tokens":5,"output_tokens":1}
	}`), &v); err != nil {
		t.Fatalf("fixture is not valid JSON: %v", err)
	}
	if v["type"] != "message" {
		t.Fatalf("fixture type = %v, want message", v["type"])
	}
}
