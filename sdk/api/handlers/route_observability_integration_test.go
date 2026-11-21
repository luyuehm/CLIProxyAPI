package handlers

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/MoeForge/moego/json"
	"github.com/gin-gonic/gin"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

// routeObsDispatchExecutor succeeds for every registered credential.
type routeObsDispatchExecutor struct {
	provider string
}

func (e *routeObsDispatchExecutor) Identifier() string { return e.provider }

func (e *routeObsDispatchExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{
		Payload: []byte(`{"id":"chatcmpl-stub","choices":[{"message":{"role":"assistant","content":"hi"}}]}`),
		Headers: http.Header{"Content-Type": {"application/json"}},
	}, nil
}

func (e *routeObsDispatchExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	chunks := make(chan coreexecutor.StreamChunk, 2)
	chunks <- coreexecutor.StreamChunk{Payload: []byte(`data: {"id":"1","choices":[{"delta":{"content":"hi"}}]}`)}
	close(chunks)
	return &coreexecutor.StreamResult{
		Headers: http.Header{"Content-Type": {"text/event-stream"}},
		Chunks:  chunks,
	}, nil
}

func (e *routeObsDispatchExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *routeObsDispatchExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, &coreauth.Error{Code: "not_implemented", Message: "CountTokens not implemented"}
}

func (e *routeObsDispatchExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, &coreauth.Error{Code: "not_implemented", Message: "HttpRequest not implemented"}
}

// routeObsFailExecutor fails every non-streaming request so the error path can
// be observed.
type routeObsFailExecutor struct {
	provider string
}

func (e *routeObsFailExecutor) Identifier() string { return e.provider }

func (e *routeObsFailExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, &coreauth.Error{Code: "upstream_503", Message: "stub upstream 503", HTTPStatus: http.StatusServiceUnavailable}
}

func (e *routeObsFailExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	return nil, &coreauth.Error{Code: "upstream_503", Message: "stub upstream 503", HTTPStatus: http.StatusServiceUnavailable}
}

func (e *routeObsFailExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *routeObsFailExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, &coreauth.Error{Code: "not_implemented", Message: "CountTokens not implemented"}
}

func (e *routeObsFailExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, &coreauth.Error{Code: "not_implemented", Message: "HttpRequest not implemented"}
}

// newRouteObsHandler boots a real manager whose registered executor serves the
// model. When failing is set, every execution fails so the error path is
// exercised.
func newRouteObsHandler(t *testing.T, model string, failing bool, stream bool) *BaseAPIHandler {
	t.Helper()
	const provider = "antigravity"
	manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
	manager.SetConfig(&internalconfig.Config{})
	manager.SetRetryConfig(0, 0, 1)
	if failing {
		manager.RegisterExecutor(&routeObsFailExecutor{provider: provider})
	} else {
		manager.RegisterExecutor(&routeObsDispatchExecutor{provider: provider})
	}

	if _, errReg := manager.Register(context.Background(), &coreauth.Auth{
		ID:         "ric3022-ok",
		Provider:   provider,
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{coreauth.AttributeAuthKind: coreauth.AuthKindOAuth},
	}); errReg != nil {
		t.Fatalf("Register: %v", errReg)
	}
	registry.GetGlobalRegistry().RegisterClient("ric3022-ok", provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient("ric3022-ok")
	})

	sc := &sdkconfig.SDKConfig{}
	if stream {
		sc.Streaming = sdkconfig.StreamingConfig{BootstrapRetries: 1}
	}
	return NewBaseAPIHandlers(sc, manager)
}

func ctxWithDebug(debug bool) context.Context {
	ginCtx, _ := gin.CreateTestContext(nil)
	path := "/v1/chat/completions"
	if debug {
		path += "?" + RouteInfoDebugQueryParam + "=true"
	}
	ginCtx.Request, _ = http.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
	return context.WithValue(context.Background(), "gin", ginCtx)
}

func TestRouteObservabilityNonStreamSuccessHeader(t *testing.T) {
	const model = "ric3022-e2e-non-ok"
	handler := newRouteObsHandler(t, model, false, false)

	body, headers, errMsg := handler.ExecuteWithAuthManager(context.Background(), "openai", model, []byte(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`), "")
	if errMsg != nil {
		t.Fatalf("ExecuteWithAuthManager err = %+v", errMsg)
	}
	if len(body) == 0 {
		t.Fatal("expected a response body")
	}

	routeHeader := headers.Get(OpenRouterRouteHeader)
	if routeHeader == "" {
		t.Fatal("X-OpenRouter-Route header missing on non-streaming success")
	}
	var parsed map[string]any
	if errDecode := json.Unmarshal([]byte(routeHeader), &parsed); errDecode != nil {
		t.Fatalf("decode X-OpenRouter-Route: %v", errDecode)
	}
	if parsed["provider"] != "antigravity" {
		t.Fatalf("provider = %v, want antigravity", parsed["provider"])
	}
	if code, _ := parsed["failure_code"].(string); code != "" {
		t.Fatalf("failure_code = %q, want empty on success", code)
	}
}

func TestRouteObservabilityNonStreamErrorHeader(t *testing.T) {
	const model = "ric3022-e2e-non-err"
	handler := newRouteObsHandler(t, model, true, false)

	_, _, errMsg := handler.ExecuteWithAuthManager(context.Background(), "openai", model, []byte(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`), "")
	if errMsg == nil {
		t.Fatal("expected an error response")
	}

	routeHeader := errMsg.Addon.Get(OpenRouterRouteHeader)
	if routeHeader == "" {
		t.Fatal("X-OpenRouter-Route header missing on non-streaming error Addon")
	}
	var parsed map[string]any
	if errDecode := json.Unmarshal([]byte(routeHeader), &parsed); errDecode != nil {
		t.Fatalf("decode X-OpenRouter-Route: %v", errDecode)
	}
	if parsed["provider"] != "antigravity" {
		t.Fatalf("provider = %v, want antigravity", parsed["provider"])
	}
	if code, _ := parsed["failure_code"].(string); code == "" {
		t.Fatal("failure_code must be present on a failed request")
	}
}

func TestRouteObservabilityNonStreamDebugBody(t *testing.T) {
	const model = "ric3022-e2e-debug-flash"
	handler := newRouteObsHandler(t, model, false, false)

	body, _, errMsg := handler.ExecuteWithAuthManager(ctxWithDebug(true), "openai", model, []byte(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`), "")
	if errMsg != nil {
		t.Fatalf("ExecuteWithAuthManager err = %+v", errMsg)
	}
	if !routeInfoContains(string(body), `"route_info"`) {
		t.Fatalf("debug response body missing route_info: %s", body)
	}
	if !routeInfoContains(string(body), `"final_provider":"antigravity"`) {
		t.Fatalf("debug response body missing final_provider: %s", body)
	}
}

func TestRouteObservabilityNonStreamNoDebugBody(t *testing.T) {
	const model = "ric3022-e2e-no-debug"
	handler := newRouteObsHandler(t, model, false, false)

	body, _, errMsg := handler.ExecuteWithAuthManager(context.Background(), "openai", model, []byte(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`), "")
	if errMsg != nil {
		t.Fatalf("ExecuteWithAuthManager err = %+v", errMsg)
	}
	if routeInfoContains(string(body), `"route_info"`) {
		t.Fatalf("non-debug response body must not contain route_info: %s", body)
	}
}

func TestRouteObservabilityStreamRouteDetailHeader(t *testing.T) {
	const model = "ric3022-e2e-stream-flash"
	handler := newRouteObsHandler(t, model, false, true)

	dataChan, headers, errChan := handler.ExecuteStreamWithAuthManager(ctxWithDebug(true), "claude", model, []byte(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`), "")
	if dataChan != nil {
		for range dataChan {
		}
	}
	for msg := range errChan {
		if msg != nil {
			t.Fatalf("stream err = %+v", msg)
		}
	}

	detail := headers.Get(RouteDetailResponseHeader)
	if detail == "" {
		t.Fatal("X-Route-Detail header missing on streaming response")
	}
	var parsed map[string]any
	if errDecode := json.Unmarshal([]byte(detail), &parsed); errDecode != nil {
		t.Fatalf("decode X-Route-Detail: %v", errDecode)
	}
	if parsed["final_provider"] != "antigravity" {
		t.Fatalf("final_provider = %v, want antigravity", parsed["final_provider"])
	}
	if _, ok := parsed["fallback_events"]; !ok {
		t.Fatalf("X-Route-Detail missing fallback_events: %v", parsed)
	}
}

func TestRouteObservabilityStreamDebugOffNoHeader(t *testing.T) {
	const model = "ric3022-e2e-stream-off"
	handler := newRouteObsHandler(t, model, false, true)

	dataChan, headers, errChan := handler.ExecuteStreamWithAuthManager(ctxWithDebug(false), "claude", model, []byte(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`), "")
	if dataChan != nil {
		for range dataChan {
		}
	}
	for msg := range errChan {
		if msg != nil {
			t.Fatalf("stream err = %+v", msg)
		}
	}
	if got := headers.Get(RouteDetailResponseHeader); got != "" {
		t.Fatalf("debug-off streaming must not emit X-Route-Detail, got %q", got)
	}
}
