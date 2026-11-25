package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/fallback"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/tidwall/gjson"
)

// routingTraceExecutor is a provider executor that always fails with the
// configured status (status 0 means "serve a successful response"). It records
// how many times it was called so a test can prove the fallback chain actually
// walked both providers.
type routingTraceExecutor struct {
	provider string
	status   int
	calls    int
	mu       sync.Mutex
}

func (e *routingTraceExecutor) Identifier() string { return e.provider }

func (e *routingTraceExecutor) Execute(_ context.Context, _ *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	if e.status == http.StatusGatewayTimeout {
		// A deadline class failure: the engine maps it to the "timeout"
		// switch reason.
		return coreexecutor.Response{}, errors.New("context deadline exceeded: upstream did not respond")
	}
	if e.status != 0 {
		return coreexecutor.Response{}, &coreauth.Error{HTTPStatus: e.status, Message: "upstream rejected the request"}
	}
	return coreexecutor.Response{Payload: []byte(`{"id":"chatcmpl-routing-trace","object":"chat.completion","model":"trace-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)}, nil
}

func (e *routingTraceExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	return nil, errors.New("streaming is not used by the routing trace tests")
}

func (e *routingTraceExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("count tokens is not used by the routing trace tests")
}

func (e *routingTraceExecutor) Refresh(_ context.Context, _ *coreauth.Auth) (*coreauth.Auth, error) {
	return nil, nil
}

func (e *routingTraceExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("http request is not used by the routing trace tests")
}

// newRoutingTraceHandler builds a handler whose model is served by the given
// executors, optionally installing a provider fallback chain in that order.
func newRoutingTraceHandler(t *testing.T, model string, cfg *sdkconfig.SDKConfig, executors []*routingTraceExecutor) *BaseAPIHandler {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetRetryConfig(0, 0, 0)
	for _, executor := range executors {
		manager.RegisterExecutor(executor)
		auth := &coreauth.Auth{
			ID:       "routing-trace-" + executor.provider,
			Provider: executor.provider,
			Status:   coreauth.StatusActive,
		}
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("manager.Register(%s): %v", executor.provider, errRegister)
		}
		registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	}
	handler := NewBaseAPIHandlers(cfg, manager)
	if len(executors) > 1 {
		chain := fallback.Config{}
		for _, executor := range executors {
			chain.Providers = append(chain.Providers, fallback.ProviderRef{Provider: executor.provider})
		}
		installed, errInstall := handler.SetFallbackEngine(chain, true)
		if errInstall != nil || !installed {
			t.Fatalf("SetFallbackEngine() installed=%v err=%v", installed, errInstall)
		}
	}
	return handler
}

// traceOf extracts metadata.routing_trace from a response body as a map.
func traceOf(t *testing.T, body []byte) map[string]any {
	t.Helper()
	raw := gjson.GetBytes(body, "metadata.routing_trace")
	if !raw.Exists() {
		t.Fatalf("metadata.routing_trace missing from body %s", body)
	}
	var trace map[string]any
	if errUnmarshal := json.Unmarshal([]byte(raw.Raw), &trace); errUnmarshal != nil {
		t.Fatalf("unmarshal routing_trace: %v", errUnmarshal)
	}
	return trace
}

// AC1: a healthy request with no fallback reports the serving provider.
func TestRoutingTraceAC1DirectSuccessReportsProvider(t *testing.T) {
	const model = "trace-model-ac1"
	providerA := &routingTraceExecutor{provider: "providera"}
	handler := newRoutingTraceHandler(t, model, &sdkconfig.SDKConfig{}, []*routingTraceExecutor{providerA})

	body, _, errMsg := handler.ExecuteWithAuthManager(context.Background(), "openai", model, []byte(`{"model":"`+model+`"}`), "")
	if errMsg != nil {
		t.Fatalf("ExecuteWithAuthManager() error = %+v", errMsg)
	}
	trace := traceOf(t, body)
	if trace["provider_id"] != "providera" {
		t.Fatalf("provider_id = %v, want providerA", trace["provider_id"])
	}
	if trace["fallback_used"] != false {
		t.Fatalf("fallback_used = %v, want false", trace["fallback_used"])
	}
	if trace["switch_reason"] != RoutingTraceSwitchReasonNone {
		t.Fatalf("switch_reason = %v, want %q", trace["switch_reason"], RoutingTraceSwitchReasonNone)
	}
	if trace["schema_version"] != RoutingTraceSchemaVersion {
		t.Fatalf("schema_version = %v, want %q", trace["schema_version"], RoutingTraceSchemaVersion)
	}
	// Summary mode must not disclose the detailed attempt trail.
	if _, present := trace["attempts"]; present {
		t.Fatalf("summary trace must not carry attempts: %v", trace)
	}
}

// AC2: a timeout on the primary provider switches to the backup and names the
// timeout as the switch reason.
func TestRoutingTraceAC2FallbackTimeoutReportsBackupProvider(t *testing.T) {
	const model = "trace-model-ac2-timeout"
	providerA := &routingTraceExecutor{provider: "providera", status: http.StatusGatewayTimeout}
	providerB := &routingTraceExecutor{provider: "providerb"}
	handler := newRoutingTraceHandler(t, model, &sdkconfig.SDKConfig{}, []*routingTraceExecutor{providerA, providerB})

	body, _, errMsg := handler.ExecuteWithAuthManager(context.Background(), "openai", model, []byte(`{"model":"`+model+`"}`), "")
	if errMsg != nil {
		t.Fatalf("ExecuteWithAuthManager() error = %+v", errMsg)
	}
	trace := traceOf(t, body)
	if trace["provider_id"] != "providerb" {
		t.Fatalf("provider_id = %v, want providerB", trace["provider_id"])
	}
	if trace["fallback_used"] != true {
		t.Fatalf("fallback_used = %v, want true", trace["fallback_used"])
	}
	if trace["switch_reason"] != "timeout" {
		t.Fatalf("switch_reason = %v, want timeout", trace["switch_reason"])
	}
	if providerA.calls == 0 || providerB.calls == 0 {
		t.Fatalf("calls A=%d B=%d, want both providers exercised", providerA.calls, providerB.calls)
	}
}

// AC2 (status variant): a 500 on the primary provider reports http_500.
func TestRoutingTraceAC2FallbackHTTP500ReportsStatusReason(t *testing.T) {
	const model = "trace-model-ac2-500"
	providerA := &routingTraceExecutor{provider: "providera", status: http.StatusInternalServerError}
	providerB := &routingTraceExecutor{provider: "providerb"}
	handler := newRoutingTraceHandler(t, model, &sdkconfig.SDKConfig{}, []*routingTraceExecutor{providerA, providerB})

	body, _, errMsg := handler.ExecuteWithAuthManager(context.Background(), "openai", model, []byte(`{"model":"`+model+`"}`), "")
	if errMsg != nil {
		t.Fatalf("ExecuteWithAuthManager() error = %+v", errMsg)
	}
	trace := traceOf(t, body)
	if trace["provider_id"] != "providerb" {
		t.Fatalf("provider_id = %v, want providerB", trace["provider_id"])
	}
	if trace["switch_reason"] != "http_500" {
		t.Fatalf("switch_reason = %v, want http_500", trace["switch_reason"])
	}
}

// AC3: a key without the detailed entitlement still gets a valid response
// carrying the summary variant, never an error and never the attempt trail.
//
// Each leg builds its own handler: the engine's HealthWindow is per-engine, so
// a first failing request puts the primary provider into cooldown and the
// second leg would skip it (no attempt trail to assert on). Independent
// handlers keep each leg's routing deterministic.
func TestRoutingTraceAC3UnlistedKeyGetsSummaryOfFullMode(t *testing.T) {
	const model = "trace-model-ac3"
	cfg := &sdkconfig.SDKConfig{RoutingTrace: config.RoutingTraceConfig{
		Mode:             config.RoutingTraceModeFull,
		FullTraceAPIKeys: []string{"paid-key"},
	}}
	newHandler := func() *BaseAPIHandler {
		providerA := &routingTraceExecutor{provider: "providera", status: http.StatusBadGateway}
		providerB := &routingTraceExecutor{provider: "providerb"}
		return newRoutingTraceHandler(t, model, cfg, []*routingTraceExecutor{providerA, providerB})
	}
	requestCtx := func(bearer string) context.Context {
		gin.SetMode(gin.TestMode)
		recorder := httptest.NewRecorder()
		ginCtx, _ := gin.CreateTestContext(recorder)
		ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		ginCtx.Request.Header.Set("Authorization", "Bearer "+bearer)
		return context.WithValue(context.Background(), "gin", ginCtx)
	}

	// A free-tier key is not listed: summary variant, no attempts, no error.
	freeBody, _, freeErr := newHandler().ExecuteWithAuthManager(requestCtx("free-key"), "openai", model, []byte(`{"model":"`+model+`"}`), "")
	if freeErr != nil {
		t.Fatalf("ExecuteWithAuthManager(free key) error = %+v", freeErr)
	}
	freeTrace := traceOf(t, freeBody)
	if _, present := freeTrace["attempts"]; present {
		t.Fatalf("free-tier trace must not carry attempts: %v", freeTrace)
	}
	if freeTrace["provider_id"] != "providerb" {
		t.Fatalf("free-tier provider_id = %v, want providerb", freeTrace["provider_id"])
	}

	// The listed key gets the detailed variant with the failed attempt.
	paidBody, _, paidErr := newHandler().ExecuteWithAuthManager(requestCtx("paid-key"), "openai", model, []byte(`{"model":"`+model+`"}`), "")
	if paidErr != nil {
		t.Fatalf("ExecuteWithAuthManager(paid key) error = %+v", paidErr)
	}
	attempts := gjson.GetBytes(paidBody, "metadata.routing_trace.attempts")
	if !attempts.Exists() || attempts.Get("0.provider_id").String() != "providera" {
		t.Fatalf("paid-tier attempts = %s, want first attempt providera (body %s)", attempts.Raw, paidBody)
	}
	if reason := attempts.Get("0.reason").String(); reason != "http_502" {
		t.Fatalf("paid-tier first attempt reason = %q, want http_502", reason)
	}
	if decisionPath := gjson.GetBytes(paidBody, "metadata.routing_trace.decision_path").Array(); len(decisionPath) < 2 || decisionPath[0].String() != "primary" || decisionPath[1].String() != "fallback" {
		t.Fatalf("decision_path = %s, want [primary fallback]", gjson.GetBytes(paidBody, "metadata.routing_trace.decision_path").Raw)
	}
	if candidates := gjson.GetBytes(paidBody, "metadata.routing_trace.candidates").Array(); len(candidates) == 0 {
		t.Fatalf("candidates missing in full mode")
	}
}

// Off mode attaches nothing and leaves the body byte-for-byte untouched.
func TestRoutingTraceOffModeLeavesBodyUntouched(t *testing.T) {
	const model = "trace-model-off"
	providerA := &routingTraceExecutor{provider: "providera"}
	cfg := &sdkconfig.SDKConfig{RoutingTrace: config.RoutingTraceConfig{Mode: config.RoutingTraceModeOff}}
	handler := newRoutingTraceHandler(t, model, cfg, []*routingTraceExecutor{providerA})

	body, _, errMsg := handler.ExecuteWithAuthManager(context.Background(), "openai", model, []byte(`{"model":"`+model+`"}`), "")
	if errMsg != nil {
		t.Fatalf("ExecuteWithAuthManager() error = %+v", errMsg)
	}
	if gjson.GetBytes(body, "metadata").Exists() {
		t.Fatalf("off mode must not add metadata: %s", body)
	}
}

func TestAttachRoutingTraceMergesIntoSiblingMetadata(t *testing.T) {
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	collector := NewRouteInfoCollector([]string{"providera"}, false)
	collector.ReportEvent(coreexecutor.RouteEvent{Provider: "providera", Model: "m", Outcome: coreexecutor.RouteOutcomeSuccess})

	body := []byte(`{"id":"x","metadata":{"existing":"value"}}`)
	updated := handler.attachRoutingTrace(context.Background(), body, collector)
	if got := gjson.GetBytes(updated, "metadata.existing").String(); got != "value" {
		t.Fatalf("sibling metadata field lost: %s", updated)
	}
	if got := gjson.GetBytes(updated, "metadata.routing_trace.provider_id").String(); got != "providera" {
		t.Fatalf("routing_trace.provider_id = %q, want providerA (%s)", got, updated)
	}
}

// A non-object metadata value is never overwritten: doing so would destroy an
// upstream field the gateway does not own.
func TestAttachRoutingTraceSkipsNonObjectMetadata(t *testing.T) {
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	collector := NewRouteInfoCollector([]string{"providera"}, false)
	collector.ReportEvent(coreexecutor.RouteEvent{Provider: "providera", Outcome: coreexecutor.RouteOutcomeSuccess})

	body := []byte(`{"id":"x","metadata":"opaque"}`)
	if got := handler.attachRoutingTrace(context.Background(), body, collector); string(got) != string(body) {
		t.Fatalf("non-object metadata was modified: %s", got)
	}
}

// Non-object and invalid bodies pass through unchanged so a non-JSON endpoint
// (or an interceptor-produced plain payload) can never be corrupted.
func TestAttachRoutingTraceSkipsNonJSONBody(t *testing.T) {
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	collector := NewRouteInfoCollector([]string{"providera"}, false)
	collector.ReportEvent(coreexecutor.RouteEvent{Provider: "providera", Outcome: coreexecutor.RouteOutcomeSuccess})

	for _, body := range [][]byte{[]byte(`not json`), []byte(`[1,2,3]`), []byte(``), nil} {
		if got := handler.attachRoutingTrace(context.Background(), body, collector); string(got) != string(body) {
			t.Fatalf("body %q was modified to %q", body, got)
		}
	}
}

// No attributable provider means no trace rather than an empty provider_id.
func TestBuildRoutingTraceRequiresAttribution(t *testing.T) {
	collector := NewRouteInfoCollector(nil, false)
	if _, ok := collector.buildRoutingTrace(context.Background(), false); ok {
		t.Fatal("empty collector must not render a trace")
	}
	var nilCollector *RouteInfoCollector
	if _, ok := nilCollector.buildRoutingTrace(context.Background(), false); ok {
		t.Fatal("nil collector must not render a trace")
	}
}

// A same-provider, same-model retry is an availability retry, not a routing
// fallback: fallback_used stays false, matching X-OpenRouter-Fallback.
func TestBuildRoutingTraceSameTargetRetryIsNotFallback(t *testing.T) {
	collector := NewRouteInfoCollector([]string{"providera"}, false)
	collector.ReportEvent(coreexecutor.RouteEvent{Provider: "providera", Model: "m", Outcome: coreexecutor.RouteOutcomeFailed, Status: 503, Code: "upstream_503"})
	collector.ReportEvent(coreexecutor.RouteEvent{Provider: "providera", Model: "m", Outcome: coreexecutor.RouteOutcomeSuccess})

	trace, ok := collector.buildRoutingTrace(context.Background(), false)
	if !ok {
		t.Fatal("trace must render")
	}
	if trace.FallbackUsed {
		t.Fatalf("fallback_used = true for a same-target retry: %+v", trace)
	}
	if trace.SwitchReason != RoutingTraceSwitchReasonNone {
		t.Fatalf("switch_reason = %q, want %q", trace.SwitchReason, RoutingTraceSwitchReasonNone)
	}
}

// The switch_reason vocabulary is pinned literally: consumers branch on these
// strings, so a reworded mapping must fail this test.
func TestRoutingTraceSwitchReasonVocabulary(t *testing.T) {
	cases := []struct {
		name  string
		event coreexecutor.RouteEvent
		want  string
	}{
		{name: "gateway timeout status", event: coreexecutor.RouteEvent{Status: 504}, want: "timeout"},
		{name: "timeout code", event: coreexecutor.RouteEvent{Code: "upstream_timeout"}, want: "timeout"},
		{name: "deadline message", event: coreexecutor.RouteEvent{Error: "context deadline exceeded"}, want: "timeout"},
		{name: "server error", event: coreexecutor.RouteEvent{Status: 500}, want: "http_500"},
		{name: "rate limited", event: coreexecutor.RouteEvent{Status: 429}, want: "http_429"},
		{name: "status from engine code", event: coreexecutor.RouteEvent{Code: "provider_openai_503"}, want: "http_503"},
		{name: "credential cooldown", event: coreexecutor.RouteEvent{Code: "provider_openai_cooled_down"}, want: "provider_openai_cooled_down"},
		{name: "no information", event: coreexecutor.RouteEvent{}, want: "provider_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := routingTraceSwitchReason(tc.event); got != tc.want {
				t.Fatalf("routingTraceSwitchReason(%+v) = %q, want %q", tc.event, got, tc.want)
			}
		})
	}
}

// Control characters in an upstream code can never reach the payload.
func TestRoutingTraceSafeCodeStripsControlCharacters(t *testing.T) {
	if got := routingTraceSafeCode("bad\r\ncode"); got != "bad__code" {
		t.Fatalf("routingTraceSafeCode() = %q, want bad__code", got)
	}
	if got := routingTraceSafeCode("   "); got != "provider_error" {
		t.Fatalf("blank code = %q, want provider_error", got)
	}
	long := strings.Repeat("x", fallbackReasonMaxLen+50)
	if got := routingTraceSafeCode(long); len(got) != fallbackReasonMaxLen {
		t.Fatalf("long code length = %d, want %d", len(got), fallbackReasonMaxLen)
	}
}

// The detailed variant bounds the attempt list so a retry loop cannot bloat
// the payload (contract: response payload size control).
func TestRoutingTraceAttemptListIsBounded(t *testing.T) {
	collector := NewRouteInfoCollector(nil, false)
	for i := 0; i < routingTraceAttemptLimit*3; i++ {
		collector.ReportEvent(coreexecutor.RouteEvent{Provider: "providera", Model: "m", Outcome: coreexecutor.RouteOutcomeFailed, Status: 503})
	}
	collector.ReportEvent(coreexecutor.RouteEvent{Provider: "providerb", Model: "m", Outcome: coreexecutor.RouteOutcomeSuccess})

	trace, ok := collector.buildRoutingTrace(context.Background(), true)
	if !ok {
		t.Fatal("trace must render")
	}
	if len(trace.Attempts) > routingTraceAttemptLimit {
		t.Fatalf("attempts = %d, want <= %d", len(trace.Attempts), routingTraceAttemptLimit)
	}
	if trace.LatencyMS < 0 {
		t.Fatalf("latency_ms = %d, want >= 0", trace.LatencyMS)
	}
}

// The summary payload stays small enough that the trace cannot meaningfully
// change network cost for a high-volume caller.
func TestRoutingTraceSummaryPayloadIsSmall(t *testing.T) {
	collector := NewRouteInfoCollector([]string{"providera", "providerb"}, false)
	collector.ReportEvent(coreexecutor.RouteEvent{Provider: "providera", Model: "m", Outcome: coreexecutor.RouteOutcomeFailed, Status: 503})
	collector.ReportEvent(coreexecutor.RouteEvent{Provider: "providerb", Model: "m", Outcome: coreexecutor.RouteOutcomeSuccess})

	trace, ok := collector.buildRoutingTrace(context.Background(), false)
	if !ok {
		t.Fatal("trace must render")
	}
	encoded, errMarshal := json.Marshal(trace)
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	if len(encoded) > 256 {
		t.Fatalf("summary trace is %d bytes, want <= 256: %s", len(encoded), encoded)
	}
}

// The trace is scoped to client-facing chat-completion protocols. The
// Responses API, the Interactions API and internal model callbacks must keep
// their bodies byte-for-byte unchanged.
func TestRoutingTraceRequestedBoundaries(t *testing.T) {
	cases := []struct {
		name     string
		protocol string
		opts     modelExecutionOptions
		want     bool
	}{
		{name: "openai chat", protocol: "openai", want: true},
		{name: "claude messages", protocol: "claude", want: true},
		{name: "gemini content", protocol: "gemini", want: true},
		{name: "case and padding", protocol: "  OpenAI  ", want: true},
		{name: "responses api", protocol: "openai-response", want: false},
		{name: "interactions api", protocol: "interactions", want: false},
		{name: "video endpoint", protocol: "openai-video", want: false},
		{name: "internal callback", protocol: "openai", opts: modelExecutionOptions{InternalSource: true}, want: false},
		{name: "empty protocol", protocol: "", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := routingTraceRequested(tc.protocol, tc.opts); got != tc.want {
				t.Fatalf("routingTraceRequested(%q, %+v) = %v, want %v", tc.protocol, tc.opts, got, tc.want)
			}
		})
	}
}
