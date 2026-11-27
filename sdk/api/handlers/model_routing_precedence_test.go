package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

// The routing gate refines only the plain model-to-provider path. A plugin or
// arbitrage router that already resolved the request outranks it, and a nested
// plugin execution excluded from routing is left alone — otherwise the gate
// would silently overwrite a decision another router made.
func TestShouldApplyModelRoutingPrecedence(t *testing.T) {
	cases := []struct {
		name     string
		decide   modelRouteDecision
		execOpts modelExecutionOptions
		want     bool
		comment  string
	}{
		{
			name:    "plain path",
			decide:  modelRouteDecision{},
			want:    true,
			comment: "no router decision: the gate may refine the model",
		},
		{
			name:    "plugin executor decided",
			decide:  modelRouteDecision{ExecutorPluginID: "plugin-a"},
			want:    false,
			comment: "the plugin router already chose the executor",
		},
		{
			name:    "arbitrage provider decided",
			decide:  modelRouteDecision{Provider: "openai"},
			want:    false,
			comment: "the arbitrage host already chose the upstream provider",
		},
		{
			name:    "router model override",
			decide:  modelRouteDecision{Model: "some-model"},
			want:    false,
			comment: "the router already rewrote the model",
		},
		{
			name:     "nested plugin execution",
			decide:   modelRouteDecision{},
			execOpts: modelExecutionOptions{SkipRouterPluginID: "plugin-a"},
			want:     false,
			comment:  "a nested execution excluded from routing must not be re-routed",
		},
		{
			name:     "provider-pinned execution",
			decide:   modelRouteDecision{},
			execOpts: modelExecutionOptions{ForcedProvider: "gemini-interactions"},
			want:     false,
			comment:  "the caller demanded one provider; substituting another provider's model would send a foreign model upstream",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldApplyModelRouting(tc.decide, tc.execOpts); got != tc.want {
				t.Fatalf("shouldApplyModelRouting() = %v, want %v (%s)", got, tc.want, tc.comment)
			}
		})
	}
}

// A nil gate leaves the model untouched and produces no error, which is what
// keeps the default build's request path byte-for-byte unchanged.
func TestApplyModelRoutingNilGate(t *testing.T) {
	handler := &BaseAPIHandler{}
	model, errMsg := handler.applyModelRouting(nil, "", []byte(`{}`), "requested-model", false)
	if errMsg != nil {
		t.Fatalf("errMsg = %+v, want nil", errMsg)
	}
	if model != "requested-model" {
		t.Fatalf("model = %q, want the requested model unchanged", model)
	}
}

// The gate stays off for endpoints and request shapes where routing would
// change behaviour the client declared:
//   - image/video endpoints, which have their own model validation;
//   - a model carrying a thinking-budget suffix: routing to another model
//     would silently drop the budget.
func TestModelRoutingRequestExcludesOutOfScopeRequests(t *testing.T) {
	handler := &BaseAPIHandler{}
	cases := []struct {
		name        string
		handlerType string
		model       string
		imageOnly   bool
		wantGateOn  bool
	}{
		{"plain chat model", "openai", "claude-sonnet-4-5", false, true},
		{"empty model", "openai", "", false, false},
		{"thinking suffix drops budget", "openai", "claude-sonnet-4-5(8192)", false, false},
		{"image endpoint by flag", "openai", "grok-imagine-image-2.0", true, false},
		{"image endpoint by handler type", "openai-image", "gpt-image-2", false, false},
		{"video endpoint by handler type", "openai-video", "grok-imagine-video", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := handler.modelRoutingRequest(tc.handlerType, []byte(`{}`), tc.model, tc.imageOnly)
			if ok != tc.wantGateOn {
				t.Fatalf("gate applied = %v, want %v", ok, tc.wantGateOn)
			}
		})
	}
}

// The candidate predicate is what stops the engine from choosing a model the
// gateway cannot serve: image-only models never pass on a text endpoint, and a
// model with no resolvable provider is excluded.
//
// "Resolvable" means the global registry knows the model — the same registry
// lookup providersForExecution uses, so the predicate can never admit a model
// the exec path would then answer 400 model_not_found for.
func TestModelRoutingAllowedFunc(t *testing.T) {
	const clientID = "routinggate-allowed-claude"
	registry.GetGlobalRegistry().RegisterClient(clientID, "claude", []*registry.ModelInfo{
		{ID: "claude-sonnet-4-5", Object: "model", OwnedBy: "claude"},
	})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(clientID) })

	handler := &BaseAPIHandler{}
	allowed := handler.modelRoutingAllowedFunc()

	if allowed("") {
		t.Error("empty model must not be selectable")
	}
	if allowed("grok-imagine-image-2.0") {
		t.Error("image-only model must not be selectable on a text endpoint")
	}
	if allowed("grok-imagine-video") || allowed("grok-imagine-video-1.5") {
		t.Error("a video model must not be selectable on a text endpoint")
	}
	// A model with no registered provider is not servable.
	if allowed("totally-unregistered-model-xyz") {
		t.Error("model with no provider must not be selectable")
	}
	// A model the registry knows is servable.
	if !allowed("claude-sonnet-4-5") {
		t.Error("a registered text model must be selectable")
	}
	// The thinking suffix does not hide the base model from the predicate.
	if !allowed("claude-sonnet-4-5(8192)") {
		t.Error("base model of a suffixed name must still be judged on its base name")
	}
}

// countingGate records how many times the gate was consulted and returns a
// fixed model. In-package so these tests can drive the unexported gate path.
type countingGate struct {
	model string
	calls int
}

func (g *countingGate) RouteRequest(_ context.Context, _ ModelRoutingRequest) ModelRoutingOutcome {
	g.calls++
	return ModelRoutingOutcome{Model: g.model}
}

// REGRESSION (blocking review finding): /v1/images/generations and
// /v1/images/edits reach the gate through the Responses-backed branch with
// handler type "openai-response" — the same type the genuine text Responses API
// uses. Neither the handler type nor the allowImageModel flag identifies it, so
// the guard must key on the request path.
func TestModelRoutingGateSkippedOnImagesViaResponsesBranch(t *testing.T) {
	const requested = "gpt-5.4-mini"
	handler := &BaseAPIHandler{}
	gate := &countingGate{model: "claude-haiku"}
	handler.SetModelRoutingGate(gate)

	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	ctx := context.WithValue(context.Background(), "gin", ginCtx)

	body := []byte(`{"model":"gpt-5.4-mini","tools":[{"type":"image_generation"}]}`)
	if _, errMsg := handler.applyModelRouting(ctx, "openai-response", body, requested, false); errMsg != nil {
		t.Fatalf("unexpected error: %+v", errMsg)
	}
	if gate.calls != 0 {
		t.Fatalf("gate calls = %d on the responses-backed image path, want 0", gate.calls)
	}

	// The same handler type on the text Responses API must still be routed.
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	if _, errMsg := handler.applyModelRouting(ctx, "openai-response", body, requested, false); errMsg != nil {
		t.Fatalf("unexpected error: %+v", errMsg)
	}
	if gate.calls != 1 {
		t.Fatalf("gate calls = %d on the text Responses API, want 1", gate.calls)
	}
}

// The path guard covers every media endpoint, including the video ones served
// through the plain execution path.
func TestIsMediaRequestPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := map[string]bool{
		"/v1/images/generations": true,
		"/v1/images/edits":       true,
		"/v1/videos":             true,
		"/v1/videos/generations": true,
		"/v1/videos/extensions":  true,
		"/v1/chat/completions":   false,
		"/v1/responses":          false,
		"/v1/messages":           false,
	}
	for path, want := range cases {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, path, nil)
		ctx := context.WithValue(context.Background(), "gin", c)
		if got := isMediaRequestPath(ctx); got != want {
			t.Errorf("isMediaRequestPath(%q) = %v, want %v", path, got, want)
		}
	}
	// No gin context (unit callers) must not panic and must not block routing.
	if isMediaRequestPath(context.Background()) {
		t.Error("a context without gin must not report a media path")
	}
}
