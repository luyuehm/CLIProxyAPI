package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

const routeInfoTestModel = "route-info-test-model"

// routeInfoTestExecutor serves a fixed response per credential. Credentials
// listed in failAuthIDs fail with HTTP 503; all others succeed.
type routeInfoTestExecutor struct {
	mu          sync.Mutex
	failAuthIDs map[string]struct{}
	// attempts records the credential order observed for the request.
	attempts []string
}

func (*routeInfoTestExecutor) Identifier() string { return "route-info-provider" }

func (e *routeInfoTestExecutor) Execute(_ context.Context, auth *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
	e.mu.Lock()
	e.attempts = append(e.attempts, auth.ID)
	_, shouldFail := e.failAuthIDs[auth.ID]
	e.mu.Unlock()
	if shouldFail {
		return coreexecutor.Response{}, &routeInfoStatusError{status: http.StatusServiceUnavailable, message: "upstream overloaded"}
	}
	return coreexecutor.Response{Payload: []byte(`{"id":"chatcmpl-route-info","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}]}`)}, nil
}

func (e *routeInfoTestExecutor) ExecuteStream(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	if _, err := e.Execute(ctx, auth, req, opts); err != nil {
		return nil, err
	}
	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{Payload: []byte(`{"id":"chatcmpl-stream","object":"chat.completion.chunk","choices":[]}`)}
	close(chunks)
	return &coreexecutor.StreamResult{Headers: http.Header{}, Chunks: chunks}, nil
}

func (*routeInfoTestExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (*routeInfoTestExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (*routeInfoTestExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

// routeInfoStatusError carries an HTTP status so the conductor classifies the
// failure as a retryable credential error.
type routeInfoStatusError struct {
	status  int
	message string
}

func (e *routeInfoStatusError) Error() string   { return e.message }
func (e *routeInfoStatusError) StatusCode() int { return e.status }

// buildRouteInfoHandler wires a handler over a manager with the given auth IDs.
func buildRouteInfoHandler(t *testing.T, authIDs []string, failAuthIDs map[string]struct{}) (*OpenAIAPIHandler, *routeInfoTestExecutor) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	executor := &routeInfoTestExecutor{failAuthIDs: failAuthIDs}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetRetryConfig(2, 0, 4)
	manager.RegisterExecutor(executor)
	for idx, authID := range authIDs {
		_ = idx
		auth := &coreauth.Auth{ID: authID, Provider: executor.Identifier(), Status: coreauth.StatusActive, Index: fmt.Sprintf("%d", idx+1)}
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("register auth %s: %v", authID, errRegister)
		}
		registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: routeInfoTestModel}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })
	}
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	return NewOpenAIAPIHandler(base), executor
}

// postChatCompletions issues a chat completion request and returns the recorder.
func postChatCompletions(h *OpenAIAPIHandler, query string, body string) *httptest.ResponseRecorder {
	router := gin.New()
	router.POST("/v1/chat/completions", h.ChatCompletions)
	target := "/v1/chat/completions"
	if query != "" {
		target += "?" + query
	}
	request := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func decodeRouteInfo(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, body)
	}
	raw, ok := payload["route_info"]
	if !ok {
		t.Fatalf("response has no route_info field: %s", body)
	}
	info, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("route_info is not an object: %v", raw)
	}
	return info
}

// Scenario 4: debug off means no route_info, and the body stays compatible.
func TestRouteInfoAbsentWithoutDebug(t *testing.T) {
	h, _ := buildRouteInfoHandler(t, []string{"auth-a"}, nil)
	recorder := postChatCompletions(h, "", `{"model":"`+routeInfoTestModel+`","messages":[{"role":"user","content":"hi"}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, exists := payload["route_info"]; exists {
		t.Fatalf("route_info must be absent without ?debug=true: %s", recorder.Body.String())
	}
	if payload["id"] != "chatcmpl-route-info" {
		t.Fatalf("response body was altered: %s", recorder.Body.String())
	}
}

// Scenario 1: basic metadata with debug on.
func TestRouteInfoBasicMetadata(t *testing.T) {
	h, _ := buildRouteInfoHandler(t, []string{"auth-a"}, nil)
	recorder := postChatCompletions(h, "debug=true", `{"model":"`+routeInfoTestModel+`","messages":[{"role":"user","content":"hi"}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	info := decodeRouteInfo(t, recorder.Body.Bytes())
	if info["final_provider"] != "route-info-provider" {
		t.Fatalf("final_provider = %v, want route-info-provider", info["final_provider"])
	}
	reason, _ := info["selection_reason"].(string)
	if strings.TrimSpace(reason) == "" {
		t.Fatalf("selection_reason must be non-empty, got %v", info["selection_reason"])
	}
}

// Scenario 2: no fallback configured, primary healthy -> 1 candidate, empty events.
func TestRouteInfoNoFallbackBoundary(t *testing.T) {
	h, _ := buildRouteInfoHandler(t, []string{"auth-a"}, nil)
	recorder := postChatCompletions(h, "debug=true", `{"model":"`+routeInfoTestModel+`","messages":[{"role":"user","content":"hi"}]}`)
	info := decodeRouteInfo(t, recorder.Body.Bytes())

	candidates, _ := info["candidates"].([]any)
	if len(candidates) != 1 {
		t.Fatalf("candidates = %v, want exactly 1", info["candidates"])
	}
	events, _ := info["fallback_events"].([]any)
	if len(events) != 0 {
		t.Fatalf("fallback_events = %v, want empty", info["fallback_events"])
	}
}

// Scenario 3: primary 503 with a fallback credential -> event recorded, fallback serves, HTTP 200.
func TestRouteInfoPrimaryFailureFallsBack(t *testing.T) {
	h, executor := buildRouteInfoHandler(t, []string{"auth-1-primary", "auth-2-backup"}, map[string]struct{}{"auth-1-primary": {}})
	recorder := postChatCompletions(h, "debug=true", `{"model":"`+routeInfoTestModel+`","messages":[{"role":"user","content":"hi"}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("final status = %d, want 200 (body=%s)", recorder.Code, recorder.Body.String())
	}
	info := decodeRouteInfo(t, recorder.Body.Bytes())

	events, _ := info["fallback_events"].([]any)
	if len(events) == 0 {
		t.Fatalf("fallback_events must be non-empty after a primary failure: %v", info)
	}
	first, _ := events[0].(map[string]any)
	if first["failed_provider"] != "route-info-provider" {
		t.Fatalf("fallback_events[0].failed_provider = %v", first["failed_provider"])
	}
	if status, _ := first["status"].(float64); int(status) != http.StatusServiceUnavailable {
		t.Fatalf("fallback_events[0].status = %v, want 503", first["status"])
	}
	if info["final_provider"] != "route-info-provider" {
		t.Fatalf("final_provider = %v", info["final_provider"])
	}
	// The failing credential must be recorded, and the retry must land on a
	// different credential of the provider.
	failedIndex, _ := first["auth_index"].(string)
	if strings.TrimSpace(failedIndex) == "" {
		t.Fatalf("fallback_events[0].auth_index must name the failed credential: %v", first)
	}
	if len(executor.attempts) < 2 {
		t.Fatalf("expected the conductor to retry another credential, attempts=%v", executor.attempts)
	}
	if executor.attempts[0] == executor.attempts[1] {
		t.Fatalf("retry reused the same credential: %v", executor.attempts)
	}
}

// Streaming: the same decision is exposed via the X-Route-Detail header.
func TestRouteInfoStreamHeader(t *testing.T) {
	h, _ := buildRouteInfoHandler(t, []string{"auth-1-primary", "auth-2-backup"}, map[string]struct{}{"auth-1-primary": {}})
	recorder := postChatCompletions(h, "debug=true", `{"model":"`+routeInfoTestModel+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	detail := recorder.Header().Get(handlers.RouteDetailResponseHeader)
	if detail == "" {
		t.Fatalf("missing %s header on streaming response", handlers.RouteDetailResponseHeader)
	}
	if !strings.Contains(detail, `"final_provider":`) {
		t.Fatalf("header payload missing final_provider: %s", detail)
	}
	if !strings.Contains(detail, "fallback_events") {
		t.Fatalf("header payload missing fallback_events: %s", detail)
	}
}

// Streaming with debug off must not emit the header.
func TestRouteInfoStreamHeaderAbsentWithoutDebug(t *testing.T) {
	h, _ := buildRouteInfoHandler(t, []string{"auth-a"}, nil)
	recorder := postChatCompletions(h, "", `{"model":"`+routeInfoTestModel+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if got := recorder.Header().Get(handlers.RouteDetailResponseHeader); got != "" {
		t.Fatalf("%s must be absent without ?debug=true, got %q", handlers.RouteDetailResponseHeader, got)
	}
}
