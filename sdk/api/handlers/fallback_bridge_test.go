package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/fallback"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

// fbExecutor is a stub ProviderExecutor that fails or succeeds per provider,
// recording which providers were attempted and whether the request payload
// (the user's message) reached each attempt.
type fbExecutor struct {
	provider string
	failWith int // HTTP status to fail with; 0 = succeed

	mu       sync.Mutex
	calls    int
	lastBody string
}

func newFBExecutor(provider string, failWith int) *fbExecutor {
	return &fbExecutor{provider: provider, failWith: failWith}
}

func (e *fbExecutor) Identifier() string { return e.provider }

func (e *fbExecutor) CallCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

func (e *fbExecutor) LastBody() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastBody
}

func (e *fbExecutor) Execute(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	e.mu.Lock()
	e.calls++
	e.lastBody = string(req.Payload)
	e.mu.Unlock()
	if e.failWith > 0 {
		return coreexecutor.Response{}, &coreauth.Error{Code: "upstream_503", Message: "upstream unavailable", HTTPStatus: e.failWith, Retryable: true}
	}
	return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e *fbExecutor) ExecuteStream(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.mu.Lock()
	e.calls++
	e.lastBody = string(req.Payload)
	e.mu.Unlock()
	if e.failWith > 0 {
		return nil, &coreauth.Error{Code: "upstream_503", Message: "upstream unavailable", HTTPStatus: e.failWith, Retryable: true}
	}
	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{Payload: []byte(`{"ok":true}`)}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *fbExecutor) Refresh(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *fbExecutor) CountTokens(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{Payload: []byte(`{"total_tokens":1}`)}, nil
}

func (e *fbExecutor) HttpRequest(ctx context.Context, auth *coreauth.Auth, req *http.Request) (*http.Response, error) {
	return nil, &coreauth.Error{Code: "not_implemented", Message: "HttpRequest not implemented"}
}

// newFBHandler builds a handler with a live auth manager, one executor per
// provider (failing or succeeding per the failWith map), and the fallback
// bridge installed from the given provider chain.
func newFBHandler(t *testing.T, providers []string, failWith map[string]int) (*BaseAPIHandler, map[string]*fbExecutor) {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	executors := make(map[string]*fbExecutor, len(providers))
	clientIDs := make([]string, 0, len(providers))
	for _, provider := range providers {
		executor := newFBExecutor(provider, failWith[provider])
		executors[provider] = executor
		manager.RegisterExecutor(executor)
		clientIDs = append(clientIDs, "fb-auth-"+provider)
		auth := &coreauth.Auth{
			ID:         "fb-auth-" + provider,
			Provider:   provider,
			Status:     coreauth.StatusActive,
			Attributes: map[string]string{"api_key": "sk-fb-" + provider},
		}
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("manager.Register(%s): %v", provider, err)
		}
	}
	// Register the model under each provider so the conductor can route it.
	const model = "fb-model"
	for i, provider := range providers {
		registry.GetGlobalRegistry().RegisterClient(clientIDs[i], provider, []*registry.ModelInfo{{ID: model, Object: "model", OwnedBy: provider}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(clientIDs[i]) })
	}

	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	cfg := fallback.Config{}
	for _, provider := range providers {
		cfg.Providers = append(cfg.Providers, fallback.ProviderRef{Provider: provider})
	}
	if installed, err := handler.SetFallbackEngine(cfg, true); err != nil || !installed {
		t.Fatalf("SetFallbackEngine installed=%v err=%v", installed, err)
	}
	return handler, executors
}

// fbModelPayload is the chat-completion body the test sends.
const fbModelPayload = `{"model":"fb-model","messages":[{"role":"user","content":"preserve this"}]}`

// AC1: preferred provider A fails with 503 → the request is served by B and
// the response carries X-Fallback-Reason: provider_a_503.
func TestFallbackAC1DegradesAndCarriesReason(t *testing.T) {
	handler, executors := newFBHandler(t, []string{"a", "b"}, map[string]int{"a": 503})
	body, headers, errMsg := handler.ExecuteWithAuthManager(context.Background(), "openai", "fb-model", []byte(fbModelPayload), "")
	if errMsg != nil {
		t.Fatalf("request failed: %+v", errMsg)
	}
	if len(body) == 0 {
		t.Fatal("expected a served response body")
	}
	if headers == nil || headers.Get(FallbackReasonHeader) == "" {
		t.Fatalf("missing %s header, got %v", FallbackReasonHeader, headers)
	}
	if got := headers.Get(FallbackReasonHeader); got != "provider_a_503" {
		t.Fatalf("%s = %q, want provider_a_503", FallbackReasonHeader, got)
	}
	// The model pair header (RIC-2983) must coexist — they are complementary.
	if got := headers.Get(OpenRouterFallbackHeader); got == "" {
		t.Fatalf("X-OpenRouter-Fallback missing on a degraded request")
	}
	if executors["a"].CallCount() != 1 || executors["b"].CallCount() != 1 {
		t.Fatalf("attempt counts a=%d b=%d, want 1/1", executors["a"].CallCount(), executors["b"].CallCount())
	}
}

// AC2 (gateway observable): with the preferred provider healthy, the request is
// served by A and the response carries no X-Fallback-Reason — the header must be
// absent, not merely different, because it is the signal a monitor alerts on.
func TestFallbackAC2PrimaryHealthyNoReasonHeader(t *testing.T) {
	handler, executors := newFBHandler(t, []string{"a", "b"}, map[string]int{})
	body, headers, errMsg := handler.ExecuteWithAuthManager(context.Background(), "openai", "fb-model", []byte(fbModelPayload), "")
	if errMsg != nil {
		t.Fatalf("request failed: %+v", errMsg)
	}
	if len(body) == 0 {
		t.Fatal("expected a served response body")
	}
	if headers != nil {
		if got := headers.Get(FallbackReasonHeader); got != "" {
			t.Fatalf("%s present on a healthy request: %q", FallbackReasonHeader, got)
		}
	}
	if executors["a"].CallCount() != 1 {
		t.Fatalf("preferred provider attempts = %d, want 1", executors["a"].CallCount())
	}
	if executors["b"].CallCount() != 0 {
		t.Fatalf("backup provider attempted %d times on a healthy primary, want 0", executors["b"].CallCount())
	}
}

// AC2 (recovery mechanism): after a provider saturates the health window and
// cools down, ObserveSuccess clears its state and the next run serves it
// directly with no X-Fallback-Reason. This is the "health recovers → stop
// degrading" half of the contract, exercised on the engine the bridge wraps.
func TestFallbackAC2HealthRecoveryStopsDegrading(t *testing.T) {
	handler, _ := newFBHandler(t, []string{"a", "b"}, map[string]int{"a": 503})
	orch := handler.FallbackBridge.orchestrator
	health := orch.Health
	underlying := func(ctx context.Context, provider, model string) error {
		if provider == "a" {
			return &fallback.ProviderError{Provider: "a", Status: 503, Err: errors.New("upstream unavailable")}
		}
		return nil
	}
	var serves []string
	// Saturate A's window: three failures cool it down.
	for i := 0; i < 3; i++ {
		out := orch.Run(context.Background(), "fb-model", fallback.RequestOptions{}, underlying)
		if !out.Success {
			t.Fatalf("run %d: B must serve: %v", i, out.Err)
		}
		serves = append(serves, out.Provider)
	}
	for _, provider := range serves {
		if provider != "b" {
			t.Fatalf("run served by %q, want b", provider)
		}
	}

	// A recovers: a success clears its cooldown.
	health.ObserveSuccess("a", time.Now())
	out := orch.Run(context.Background(), "fb-model", fallback.RequestOptions{}, func(ctx context.Context, provider, model string) error { return nil })
	if !out.Success || out.Provider != "a" {
		t.Fatalf("after recovery the preferred provider must serve: provider=%q success=%v err=%v", out.Provider, out.Success, out.Err)
	}
	if reason := reasonHeader(fallbackAttempt{Ran: true, Outcome: out}); reason != "" {
		t.Fatalf("recovered request carries %s=%q, want absent", FallbackReasonHeader, reason)
	}
}

// AC3: A/B/C all fail → 503 with attempted_models [a,b,c] in the error body.
func TestFallbackAC3AllFailedCarriesAttemptedModels(t *testing.T) {
	handler, executors := newFBHandler(t, []string{"a", "b", "c"}, map[string]int{"a": 503, "b": 503, "c": 503})
	_, _, errMsg := handler.ExecuteWithAuthManager(context.Background(), "openai", "fb-model", []byte(fbModelPayload), "")
	if errMsg == nil {
		t.Fatal("all-providers-failed must error")
	}
	if errMsg.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", errMsg.StatusCode)
	}
	if !strings.Contains(string(errMsg.Body), "attempted_models") {
		t.Fatalf("body missing attempted_models: %s", errMsg.Body)
	}
	var parsed struct {
		Error struct {
			AttemptedModels []string `json:"attempted_models"`
		} `json:"error"`
	}
	if err := json.Unmarshal(errMsg.Body, &parsed); err != nil {
		t.Fatalf("unmarshal error body: %v", err)
	}
	want := []string{"a", "b", "c"}
	if len(parsed.Error.AttemptedModels) != len(want) {
		t.Fatalf("attempted_models = %v, want %v", parsed.Error.AttemptedModels, want)
	}
	for i, model := range want {
		if parsed.Error.AttemptedModels[i] != model {
			t.Fatalf("attempted_models = %v, want %v", parsed.Error.AttemptedModels, want)
		}
	}
	for _, executor := range executors {
		if executor.CallCount() != 1 {
			t.Fatalf("provider %s attempted %d times, want exactly 1", executor.provider, executor.CallCount())
		}
	}
}

// Context preservation: the request payload (the user's conversation) reaches
// every provider attempt unchanged — fallback switches providers, not requests.
func TestFallbackPayloadPreservedAcrossAttempts(t *testing.T) {
	handler, executors := newFBHandler(t, []string{"a", "b"}, map[string]int{"a": 503})
	_, _, errMsg := handler.ExecuteWithAuthManager(context.Background(), "openai", "fb-model", []byte(fbModelPayload), "")
	if errMsg != nil {
		t.Fatalf("request failed: %+v", errMsg)
	}
	for _, provider := range []string{"a", "b"} {
		if got := executors[provider].LastBody(); got != fbModelPayload {
			t.Fatalf("payload for %s = %q, want the original user message", provider, got)
		}
	}
}

// A single-provider chain never engages the engine: a failure keeps its
// native status and no X-Fallback-Reason is emitted.
func TestFallbackSingleProviderKeepsNativeError(t *testing.T) {
	handler, _ := newFBHandler(t, []string{"a"}, map[string]int{"a": 503})
	_, _, errMsg := handler.ExecuteWithAuthManager(context.Background(), "openai", "fb-model", []byte(fbModelPayload), "")
	if errMsg == nil {
		t.Fatal("single-provider failure must error")
	}
	if errMsg.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want native 503 (no engine rewrite)", errMsg.StatusCode)
	}
	if strings.Contains(string(errMsg.Body), "attempted_models") {
		t.Fatalf("single-provider error must not carry attempted_models: %s", errMsg.Body)
	}
}

// Stream path: a degraded stream carries X-Fallback-Reason on the upstream
// headers, and the engine walks providers before the stream starts.
func TestFallbackStreamDegradesAndCarriesReason(t *testing.T) {
	handler, executors := newFBHandler(t, []string{"a", "b"}, map[string]int{"a": 503})
	ctx := context.Background()
	dataChan, headers, errChan := handler.ExecuteStreamWithAuthManager(ctx, "openai", "fb-model", []byte(`{"model":"fb-model","stream":true}`), "")
	if headers == nil || headers.Get(FallbackReasonHeader) == "" {
		t.Fatalf("stream missing %s header, got %v", FallbackReasonHeader, headers)
	}
	if got := headers.Get(FallbackReasonHeader); got != "provider_a_503" {
		t.Fatalf("stream %s = %q, want provider_a_503", FallbackReasonHeader, got)
	}
	if executors["a"].CallCount() != 1 || executors["b"].CallCount() != 1 {
		t.Fatalf("stream attempt counts a=%d b=%d, want 1/1", executors["a"].CallCount(), executors["b"].CallCount())
	}
	// Drain the data channel so the stream goroutine is not blocked, then the
	// error channel must be clean (no terminal failure).
	for range dataChan {
	}
	if errMsg, ok := <-errChan; ok && errMsg != nil {
		t.Fatalf("stream unexpectedly errored: %+v", errMsg)
	}
}
