package handlers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	handlers "github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

// routingGateExecutor records the model each execution was asked to run.
type routingGateExecutor struct {
	provider string
	models   []string
	err      error
}

func (e *routingGateExecutor) Identifier() string { return e.provider }

func (e *routingGateExecutor) Execute(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	e.models = append(e.models, req.Model)
	if e.err != nil {
		return coreexecutor.Response{}, e.err
	}
	return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e *routingGateExecutor) ExecuteStream(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.models = append(e.models, req.Model)
	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{Payload: []byte(`{"ok":true}`)}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *routingGateExecutor) CountTokens(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	e.models = append(e.models, req.Model)
	return coreexecutor.Response{Payload: []byte(`{"total_tokens":1}`)}, nil
}

func (e *routingGateExecutor) HttpRequest(ctx context.Context, auth *coreauth.Auth, req *http.Request) (*http.Response, error) {
	return nil, &coreauth.Error{Code: "not_implemented", Message: "HttpRequest not implemented"}
}

func (e *routingGateExecutor) Refresh(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

// stubRoutingGate returns a fixed outcome, so the handler-side wiring is
// exercised without the modelrouting engine.
type stubRoutingGate struct {
	model       string
	headers     http.Header
	err         *int
	calls       int
	lastRequest handlers.ModelRoutingRequest
}

func (g *stubRoutingGate) RouteRequest(ctx context.Context, req handlers.ModelRoutingRequest) handlers.ModelRoutingOutcome {
	g.calls++
	g.lastRequest = req
	out := handlers.ModelRoutingOutcome{Model: g.model, Headers: g.headers}
	if g.err != nil {
		out.Error = routingErrorMessage(*g.err)
	}
	return out
}

func routingErrorMessage(status int) *interfaces.ErrorMessage {
	body, _ := json.Marshal(map[string]any{"error": map[string]any{"code": "budget_exceeded"}})
	return &interfaces.ErrorMessage{
		StatusCode:     status,
		DirectResponse: true,
		Body:           body,
	}
}

func newRoutingGateHandler(t *testing.T, models []string, key string) (*handlers.BaseAPIHandler, *routingGateExecutor, *gin.Context) {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	executor := &routingGateExecutor{provider: "claude"}
	manager.RegisterExecutor(executor)

	const clientID = "routinggate-auth-claude"
	infos := make([]*registry.ModelInfo, 0, len(models))
	for _, model := range models {
		infos = append(infos, &registry.ModelInfo{ID: model, Object: "model", OwnedBy: "claude"})
	}
	registry.GetGlobalRegistry().RegisterClient(clientID, "claude", infos)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(clientID) })

	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:         clientID,
		Provider:   "claude",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"source_backend": "objectstore", "api_key": "sk-managed-claude"},
	}); err != nil {
		t.Fatalf("manager.Register: %v", err)
	}

	handler := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	request, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	request.Header.Set("Authorization", "Bearer "+key)
	ginCtx.Request = request
	ginCtx.Set("userApiKey", key)
	return handler, executor, ginCtx
}

// AC1: with no gate installed the request path is unchanged — the requested
// model reaches the executor and no extra header appears.
func TestModelRoutingAbsentGateIsNoOp(t *testing.T) {
	const model = "routinggate-plain"
	handler, executor, ginCtx := newRoutingGateHandler(t, []string{model}, "sk-plain")
	ctx := context.WithValue(context.Background(), "gin", ginCtx)

	_, _, errMsg := handler.ExecuteWithAuthManager(ctx, "openai", model, []byte(`{"model":"`+model+`"}`), "")
	if errMsg != nil {
		t.Fatalf("ExecuteWithAuthManager error: %+v", errMsg)
	}
	if len(executor.models) != 1 || executor.models[0] != model {
		t.Fatalf("executor saw models %v, want [%s] (unchanged)", executor.models, model)
	}
	if got := ginCtx.Writer.Header().Get("x-or-model"); got != "" {
		t.Fatalf("x-or-model = %q, want empty without a gate", got)
	}
}

// AC2 wiring: an installed gate that selects another model makes that model
// reach the executor, and its headers land on the response.
func TestModelRoutingGateSwapsExecutedModel(t *testing.T) {
	const requested = "routinggate-requested"
	handler, executor, ginCtx := newRoutingGateHandler(t, []string{requested, "claude-haiku"}, "sk-gated")

	headers := make(http.Header)
	headers.Set("x-or-model", "claude-haiku")
	headers.Set("x-or-reason", "chose_claude-haiku: quality_score=0.30 cost=$0.0001")
	gate := &stubRoutingGate{model: "claude-haiku", headers: headers}
	handler.SetModelRoutingGate(gate)

	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	_, _, errMsg := handler.ExecuteWithAuthManager(ctx, "openai", requested, []byte(`{"model":"`+requested+`"}`), "")
	if gate.calls != 1 {
		t.Fatalf("gate calls = %d, want 1", gate.calls)
	}
	if errMsg != nil {
		t.Fatalf("ExecuteWithAuthManager error: %+v", errMsg)
	}
	// The gate's chosen model is registered, so the executor must see it —
	// a non-empty executor.models pins that the swap reached execution, not
	// merely the response header.
	if got := ginCtx.Writer.Header().Get("x-or-model"); got != "claude-haiku" {
		t.Fatalf("x-or-model = %q, want claude-haiku", got)
	}
	if len(executor.models) == 0 {
		t.Fatal("executor never ran; cannot prove the routed model was applied")
	}
	for _, seen := range executor.models {
		if seen != "claude-haiku" {
			t.Fatalf("executor ran %q, want the gate-selected claude-haiku", seen)
		}
	}
}

// The gate must reach the streaming path too: it carries most chat traffic, and
// a regression there would be silent. Same stub as the non-stream case; the
// executor records the model its ExecuteStream was given.
func TestModelRoutingGateAppliesOnStreamPath(t *testing.T) {
	const requested = "routinggate-stream-requested"
	handler, executor, ginCtx := newRoutingGateHandler(t, []string{requested, "claude-haiku"}, "sk-stream")

	headers := make(http.Header)
	headers.Set("x-or-model", "claude-haiku")
	handler.SetModelRoutingGate(&stubRoutingGate{model: "claude-haiku", headers: headers})

	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	_, _, errCh := handler.ExecuteStreamWithAuthManager(ctx, "openai", requested, []byte(`{"model":"`+requested+`"}`), "")
	if errCh != nil {
		select {
		case errMsg := <-errCh:
			if errMsg != nil {
				t.Fatalf("stream execution error: %+v", errMsg)
			}
		default:
		}
	}
	if got := ginCtx.Writer.Header().Get("x-or-model"); got != "claude-haiku" {
		t.Fatalf("x-or-model = %q, want claude-haiku (gate did not run on the stream path)", got)
	}
	if len(executor.models) == 0 {
		t.Fatal("stream executor never ran; cannot prove the routed model was applied")
	}
	for _, seen := range executor.models {
		if seen != "claude-haiku" {
			t.Fatalf("stream executor ran %q, want the gate-selected claude-haiku", seen)
		}
	}
}

// The count path routes too. Mutating the count call site away must fail this.
func TestModelRoutingGateAppliesOnCountPath(t *testing.T) {
	const requested = "routinggate-count-requested"
	handler, executor, ginCtx := newRoutingGateHandler(t, []string{requested, "claude-haiku"}, "sk-count")

	headers := make(http.Header)
	headers.Set("x-or-model", "claude-haiku")
	handler.SetModelRoutingGate(&stubRoutingGate{model: "claude-haiku", headers: headers})

	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	_, _, errMsg := handler.ExecuteCountWithAuthManager(ctx, "openai", requested, []byte(`{"model":"`+requested+`"}`), "")
	if errMsg != nil {
		t.Fatalf("count execution error: %+v", errMsg)
	}
	if got := ginCtx.Writer.Header().Get("x-or-model"); got != "claude-haiku" {
		t.Fatalf("x-or-model = %q, want claude-haiku (gate did not run on the count path)", got)
	}
	if len(executor.models) == 0 {
		t.Fatal("count executor never ran; cannot prove the routed model was applied")
	}
	for _, seen := range executor.models {
		if seen != "claude-haiku" {
			t.Fatalf("count executor ran %q, want the gate-selected claude-haiku", seen)
		}
	}
}

// The gate must NOT run on an image endpoint: those have their own model
// validation and a text price table has nothing to say about them.
func TestModelRoutingGateSkippedOnImageEndpoint(t *testing.T) {
	const requested = "routinggate-image-requested"
	handler, _, ginCtx := newRoutingGateHandler(t, []string{requested}, "sk-image")
	gate := &stubRoutingGate{model: "claude-haiku"}
	handler.SetModelRoutingGate(gate)

	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	// ExecuteImageWithAuthManager routes with allowImageModel = true.
	_, _, _ = handler.ExecuteImageWithAuthManager(ctx, "openai", requested, []byte(`{"model":"`+requested+`","prompt":"x"}`), "")
	if gate.calls != 0 {
		t.Fatalf("gate calls = %d on an image endpoint, want 0", gate.calls)
	}
}

// REGRESSION (blocking review finding): the video endpoints call the plain
// ExecuteWithAuthManager path (they do not set allowImageModel), so a
// flag-only guard silently routed video requests — replacing a video model
// with a chat model and dispatching the job to it. The exclusion must key on
// the handler type.
func TestModelRoutingGateSkippedOnVideoEndpoint(t *testing.T) {
	const requested = "grok-imagine-video"
	handler, _, ginCtx := newRoutingGateHandler(t, []string{requested}, "sk-video")
	gate := &stubRoutingGate{model: "claude-haiku"}
	handler.SetModelRoutingGate(gate)

	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	// "openai-video" is the handler type the /v1/videos sites pass.
	_, _, _ = handler.ExecuteWithAuthManager(ctx, "openai-video", requested, []byte(`{"model":"`+requested+`","prompt":"x"}`), "")
	if gate.calls != 0 {
		t.Fatalf("gate calls = %d on a video endpoint, want 0", gate.calls)
	}
}

// A terminal gate outcome (402) is returned before any executor runs.
func TestModelRoutingGateTerminalErrorSkipsUpstream(t *testing.T) {
	const model = "routinggate-budget"
	handler, executor, ginCtx := newRoutingGateHandler(t, []string{model}, "sk-budget")
	status := http.StatusPaymentRequired
	handler.SetModelRoutingGate(&stubRoutingGate{err: &status})

	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	_, _, errMsg := handler.ExecuteWithAuthManager(ctx, "openai", model, []byte(`{"model":"`+model+`"}`), "")
	if errMsg == nil {
		t.Fatal("expected the gate's terminal error")
	}
	if errMsg.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402", errMsg.StatusCode)
	}
	if len(executor.models) != 0 {
		t.Fatalf("executor ran %v despite a terminal routing rejection", executor.models)
	}
}

// A nil gate stored explicitly disables the channel.
func TestSetModelRoutingGateNilClears(t *testing.T) {
	const model = "routinggate-clear"
	handler, _, _ := newRoutingGateHandler(t, []string{model}, "sk-clear")
	handler.SetModelRoutingGate(&stubRoutingGate{model: "x"})
	handler.SetModelRoutingGate(nil)
	if handler.ModelRoutingGate != nil {
		t.Fatal("nil gate must clear ModelRoutingGate")
	}
}
