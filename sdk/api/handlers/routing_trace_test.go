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
	updated := handler.attachRoutingTrace(context.Background(), body, collector, nil)
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
	if got := handler.attachRoutingTrace(context.Background(), body, collector, nil); string(got) != string(body) {
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
		if got := handler.attachRoutingTrace(context.Background(), body, collector, nil); string(got) != string(body) {
			t.Fatalf("body %q was modified to %q", body, got)
		}
	}
}

// No attributable provider means no trace rather than an empty provider_id.
func TestBuildRoutingTraceRequiresAttribution(t *testing.T) {
	collector := NewRouteInfoCollector(nil, false)
	if _, ok := collector.buildRoutingTrace(context.Background(), false, nil); ok {
		t.Fatal("empty collector must not render a trace")
	}
	var nilCollector *RouteInfoCollector
	if _, ok := nilCollector.buildRoutingTrace(context.Background(), false, nil); ok {
		t.Fatal("nil collector must not render a trace")
	}
}

// A same-provider, same-model retry is an availability retry, not a routing
// fallback: fallback_used stays false, matching X-OpenRouter-Fallback.
func TestBuildRoutingTraceSameTargetRetryIsNotFallback(t *testing.T) {
	collector := NewRouteInfoCollector([]string{"providera"}, false)
	collector.ReportEvent(coreexecutor.RouteEvent{Provider: "providera", Model: "m", Outcome: coreexecutor.RouteOutcomeFailed, Status: 503, Code: "upstream_503"})
	collector.ReportEvent(coreexecutor.RouteEvent{Provider: "providera", Model: "m", Outcome: coreexecutor.RouteOutcomeSuccess})

	trace, ok := collector.buildRoutingTrace(context.Background(), false, nil)
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

	trace, ok := collector.buildRoutingTrace(context.Background(), true, nil)
	if !ok {
		t.Fatal("trace must render")
	}
	// Pinned literally, not against routingTraceAttemptLimit: comparing to the
	// constant the code uses would pass even if the constant itself drifted.
	const wantBound = 16
	if len(trace.Attempts) != wantBound {
		t.Fatalf("attempts = %d, want exactly %d (the documented bound)", len(trace.Attempts), wantBound)
	}
	if routingTraceAttemptLimit != wantBound {
		t.Fatalf("routingTraceAttemptLimit = %d, want the documented %d", routingTraceAttemptLimit, wantBound)
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

	trace, ok := collector.buildRoutingTrace(context.Background(), false, nil)
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

// The engine reports a switch that never produced a route event: a provider
// already cooling down is skipped without invoking its executor, so the
// collector alone would report fallback_used=false while X-Fallback-Reason
// reports the switch. The trace must follow the engine.
func TestBuildRoutingTraceEngineSwitchWithoutEvents(t *testing.T) {
	collector := NewRouteInfoCollector([]string{"providera", "providerb"}, false)
	collector.ReportEvent(coreexecutor.RouteEvent{Provider: "providerb", Model: "m", Outcome: coreexecutor.RouteOutcomeSuccess})

	state := &fallbackOutcomeState{}
	state.set(fallbackAttempt{Ran: true, Outcome: fallback.Outcome{
		Success:  true,
		Provider: "providerb",
		Fallbacks: []fallback.FallbackRecord{
			{From: "providera", To: "providerb", Reason: "provider_providera_500"},
		},
	}})

	trace, ok := collector.buildRoutingTrace(context.Background(), false, state)
	if !ok {
		t.Fatal("trace must render")
	}
	if !trace.FallbackUsed {
		t.Fatalf("fallback_used = false for an engine switch: %+v", trace)
	}
	if trace.SwitchReason != "http_500" {
		t.Fatalf("switch_reason = %q, want http_500 (from the engine code)", trace.SwitchReason)
	}
	if trace.ProviderID != "providerb" {
		t.Fatalf("provider_id = %q, want providerb", trace.ProviderID)
	}
}

// A provider that is merely cooling down reports the engine's own code when no
// status can be recovered.
func TestBuildRoutingTraceEngineCooldownReason(t *testing.T) {
	collector := NewRouteInfoCollector(nil, false)
	collector.ReportEvent(coreexecutor.RouteEvent{Provider: "providerb", Model: "m", Outcome: coreexecutor.RouteOutcomeSuccess})

	state := &fallbackOutcomeState{}
	state.set(fallbackAttempt{Ran: true, Outcome: fallback.Outcome{
		Success:   true,
		Provider:  "providerb",
		Fallbacks: []fallback.FallbackRecord{{From: "providera", To: "providerb", Reason: "provider_providera_cooled_down"}},
	}})

	trace, ok := collector.buildRoutingTrace(context.Background(), false, state)
	if !ok || !trace.FallbackUsed {
		t.Fatalf("trace = %+v ok=%v, want a fallback", trace, ok)
	}
	if trace.SwitchReason != "provider_providera_cooled_down" {
		t.Fatalf("switch_reason = %q, want the engine code", trace.SwitchReason)
	}
}

// A multi-hop chain reports the hop that actually caused the serving switch
// (the same record X-Fallback-Reason reads), not the first failure.
func TestBuildRoutingTraceMultiHopReportsServingHop(t *testing.T) {
	collector := NewRouteInfoCollector([]string{"providera", "providerb", "providerc"}, false)
	collector.ReportEvent(coreexecutor.RouteEvent{Provider: "providera", Model: "m", Outcome: coreexecutor.RouteOutcomeFailed, Status: 500})
	collector.ReportEvent(coreexecutor.RouteEvent{Provider: "providerb", Model: "m", Outcome: coreexecutor.RouteOutcomeFailed, Status: 504})
	collector.ReportEvent(coreexecutor.RouteEvent{Provider: "providerc", Model: "m", Outcome: coreexecutor.RouteOutcomeSuccess})

	state := &fallbackOutcomeState{}
	state.set(fallbackAttempt{Ran: true, Outcome: fallback.Outcome{
		Success:  true,
		Provider: "providerc",
		Fallbacks: []fallback.FallbackRecord{
			{From: "providera", To: "providerb", Reason: "provider_providera_500"},
			{From: "providerb", To: "providerc", Reason: "provider_providerb_504"},
		},
	}})

	trace, ok := collector.buildRoutingTrace(context.Background(), true, state)
	if !ok {
		t.Fatal("trace must render")
	}
	// The serving hop is providerb -> providerc, and providerb timed out.
	if trace.SwitchReason != "timeout" {
		t.Fatalf("switch_reason = %q, want timeout (the serving hop's reason)", trace.SwitchReason)
	}
	if len(trace.Attempts) != 2 || trace.Attempts[0].ProviderID != "providera" || trace.Attempts[1].ProviderID != "providerb" {
		t.Fatalf("attempts = %+v, want [providera providerb] in order", trace.Attempts)
	}
}

// An engine same-target retry record (From == To) is not a switch.
func TestRoutingTraceEngineSameTargetRecordIsNotSwitch(t *testing.T) {
	state := &fallbackOutcomeState{}
	state.set(fallbackAttempt{Ran: true, Outcome: fallback.Outcome{
		Success:   true,
		Provider:  "providera",
		Fallbacks: []fallback.FallbackRecord{{From: "providera", To: "providera", Reason: "provider_providera_503"}},
	}})
	if _, _, _, switched := routingTraceEngineSwitch(state); switched {
		t.Fatal("From == To was treated as a switch")
	}
}

// The full-trace entitlement follows the same credential spellings the access
// layer accepts; a narrower extraction would silently downgrade an entitled
// key that authenticated through another spelling.
func TestRoutingTraceCallerKeyAcceptsEveryAccessSpelling(t *testing.T) {
	build := func(header, value string, query string) context.Context {
		gin.SetMode(gin.TestMode)
		ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
		target := "/v1/chat/completions"
		if query != "" {
			target += "?" + query
		}
		ginCtx.Request = httptest.NewRequest(http.MethodPost, target, nil)
		if header != "" {
			ginCtx.Request.Header.Set(header, value)
		}
		return context.WithValue(context.Background(), "gin", ginCtx)
	}
	cases := []struct {
		name string
		ctx  context.Context
		want string
	}{
		{name: "bearer", ctx: build("Authorization", "Bearer paid-key", ""), want: "paid-key"},
		{name: "raw authorization", ctx: build("Authorization", "paid-key", ""), want: "paid-key"},
		{name: "x-api-key", ctx: build("X-Api-Key", "paid-key", ""), want: "paid-key"},
		{name: "x-goog-api-key", ctx: build("X-Goog-Api-Key", "paid-key", ""), want: "paid-key"},
		{name: "query key", ctx: build("", "", "key=paid-key"), want: "paid-key"},
		{name: "query auth_token", ctx: build("", "", "auth_token=paid-key"), want: "paid-key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := routingTraceCallerKey(tc.ctx); got != tc.want {
				t.Fatalf("routingTraceCallerKey() = %q, want %q", got, tc.want)
			}
		})
	}
}

// A gzipped upstream body keeps its trace: the body is decompressed, merged,
// and recompressed rather than silently skipped.
func TestAttachRoutingTraceMergesGzipBody(t *testing.T) {
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	collector := NewRouteInfoCollector([]string{"providera"}, false)
	collector.ReportEvent(coreexecutor.RouteEvent{Provider: "providera", Model: "m", Outcome: coreexecutor.RouteOutcomeSuccess})

	plain := []byte(`{"id":"x","object":"chat.completion"}`)
	compressed, errCompress := routingTraceCompress(plain)
	if errCompress != nil {
		t.Fatalf("compress: %v", errCompress)
	}
	updated := handler.attachRoutingTrace(context.Background(), compressed, collector, nil)
	if len(updated) < 2 || updated[0] != 0x1f || updated[1] != 0x8b {
		t.Fatalf("result is not gzip: %x", updated[:min(len(updated), 2)])
	}
	decompressed, wasCompressed := routingTraceDecompress(updated)
	if !wasCompressed {
		t.Fatal("result did not decompress")
	}
	if got := gjson.GetBytes(decompressed, "metadata.routing_trace.provider_id").String(); got != "providera" {
		t.Fatalf("provider_id after round trip = %q, want providera (%s)", got, decompressed)
	}
	if got := gjson.GetBytes(decompressed, "object").String(); got != "chat.completion" {
		t.Fatalf("sibling field lost: %s", decompressed)
	}
}

// A gzip body that is not valid JSON is left byte-for-byte untouched (never
// replaced with something we invented).
func TestAttachRoutingTraceLeavesNonJSONGzipUntouched(t *testing.T) {
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	collector := NewRouteInfoCollector([]string{"providera"}, false)
	collector.ReportEvent(coreexecutor.RouteEvent{Provider: "providera", Outcome: coreexecutor.RouteOutcomeSuccess})

	compressed, errCompress := routingTraceCompress([]byte(`not json`))
	if errCompress != nil {
		t.Fatalf("compress: %v", errCompress)
	}
	if got := handler.attachRoutingTrace(context.Background(), compressed, collector, nil); string(got) != string(compressed) {
		t.Fatal("non-JSON gzip body was modified")
	}
}

// routingTraceEngineSwitch must pick the committed switch INTO the serving
// provider — the exact rule X-Fallback-Reason uses (`record.To == Provider`).
// Without that filter a trailing record describing a different move would win
// and the body would name a hop the header does not.
func TestRoutingTraceEngineSwitchSkipsNonServingRecords(t *testing.T) {
	state := &fallbackOutcomeState{}
	state.set(fallbackAttempt{Ran: true, Outcome: fallback.Outcome{
		Success:  true,
		Provider: "providerb",
		Fallbacks: []fallback.FallbackRecord{
			{From: "providera", To: "providerb", Reason: "provider_providera_500"},
			// A record that moves between two providers neither of which
			// served the request.
			{From: "providerx", To: "providery", Reason: "provider_providerx_503"},
		},
	}})

	provider, from, reason, switched := routingTraceEngineSwitch(state)
	if !switched {
		t.Fatal("switch not detected")
	}
	if provider != "providerb" || from != "providera" || reason != "provider_providera_500" {
		t.Fatalf("switch = (%q, %q, %q), want (providerb, providera, provider_providera_500)", provider, from, reason)
	}
}

// The entitlement must follow the principal that actually authenticated. A
// stale Authorization header beside a valid X-Api-Key must not decide the
// disclosure level, and a listed key must not win over a different key that
// really authenticated.
func TestRoutingTraceCallerKeyPrefersAuthenticatedPrincipal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ginCtx.Request.Header.Set("Authorization", "Bearer stale-key")
	ginCtx.Request.Header.Set("X-Api-Key", "listed-key")
	ginCtx.Set("userApiKey", "listed-key")

	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	if got := routingTraceCallerKey(ctx); got != "listed-key" {
		t.Fatalf("routingTraceCallerKey() = %q, want the authenticated principal listed-key", got)
	}
}

// Without an access manager (auth disabled) the fallback extraction must use
// the access layer's precedence: Authorization, then X-Goog-Api-Key, then
// X-Api-Key, then the query parameters.
func TestRoutingTraceCallerKeyFallbackPrecedence(t *testing.T) {
	build := func(headers map[string]string, query string) context.Context {
		gin.SetMode(gin.TestMode)
		ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
		target := "/v1/chat/completions"
		if query != "" {
			target += "?" + query
		}
		ginCtx.Request = httptest.NewRequest(http.MethodPost, target, nil)
		for name, value := range headers {
			ginCtx.Request.Header.Set(name, value)
		}
		return context.WithValue(context.Background(), "gin", ginCtx)
	}
	cases := []struct {
		name string
		ctx  context.Context
		want string
	}{
		{name: "authorization wins", ctx: build(map[string]string{"Authorization": "Bearer a", "X-Api-Key": "b", "X-Goog-Api-Key": "c"}, ""), want: "a"},
		{name: "goog before api key", ctx: build(map[string]string{"X-Api-Key": "b", "X-Goog-Api-Key": "c"}, ""), want: "c"},
		{name: "headers before query", ctx: build(map[string]string{"X-Api-Key": "b"}, "key=q"), want: "b"},
		{name: "query key before auth_token", ctx: build(nil, "key=q&auth_token=t"), want: "q"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := routingTraceCallerKey(tc.ctx); got != tc.want {
				t.Fatalf("routingTraceCallerKey() = %q, want %q", got, tc.want)
			}
		})
	}
}

// An engine provider that nothing corroborates must not overwrite the
// provider the route events attributed.
func TestBuildRoutingTraceRejectsUncorroboratedEngineProvider(t *testing.T) {
	collector := NewRouteInfoCollector([]string{"providera"}, false)
	collector.ReportEvent(coreexecutor.RouteEvent{Provider: "providera", Model: "m", Outcome: coreexecutor.RouteOutcomeSuccess})

	state := &fallbackOutcomeState{}
	state.set(fallbackAttempt{Ran: true, Outcome: fallback.Outcome{Success: true, Provider: "ghost"}})

	trace, ok := collector.buildRoutingTrace(context.Background(), false, state)
	if !ok {
		t.Fatal("trace must render")
	}
	if trace.ProviderID != "providera" {
		t.Fatalf("provider_id = %q, want providera (the only corroborated provider)", trace.ProviderID)
	}
	if trace.FallbackUsed {
		t.Fatal("an uncorroborated engine provider must not imply a fallback")
	}
}

// A JSON null metadata carries no content, so the trace may replace it instead
// of dropping the field entirely.
func TestAttachRoutingTraceReplacesNullMetadata(t *testing.T) {
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	collector := NewRouteInfoCollector([]string{"providera"}, false)
	collector.ReportEvent(coreexecutor.RouteEvent{Provider: "providera", Outcome: coreexecutor.RouteOutcomeSuccess})

	updated := handler.attachRoutingTrace(context.Background(), []byte(`{"id":"x","metadata":null}`), collector, nil)
	if got := gjson.GetBytes(updated, "metadata.routing_trace.provider_id").String(); got != "providera" {
		t.Fatalf("null metadata not replaced: %s", updated)
	}
}

// An access layer that authenticated without naming a principal must not fall
// through to header extraction: else a listed key in a header it never
// authenticated would grant the detailed variant.
func TestRoutingTraceCallerKeyEmptyPrincipalFailsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ginCtx.Request.Header.Set("Authorization", "Bearer listed-key")
	ginCtx.Set("userApiKey", "")

	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	if got := routingTraceCallerKey(ctx); got != "" {
		t.Fatalf("routingTraceCallerKey() = %q, want empty (fail closed on an empty principal)", got)
	}
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{RoutingTrace: config.RoutingTraceConfig{
		Mode:             config.RoutingTraceModeFull,
		FullTraceAPIKeys: []string{"listed-key"},
	}}, nil)
	if mode := handler.routingTraceMode(ctx); mode != config.RoutingTraceModeSummary {
		t.Fatalf("routingTraceMode() = %q, want summary", mode)
	}
}

// A provider named only by a failed attempt did not serve the request.
func TestRoutingTraceNamesProviderRequiresSuccess(t *testing.T) {
	events := []coreexecutor.RouteEvent{
		{Provider: "ghost", Outcome: coreexecutor.RouteOutcomeFailed},
		{Provider: "providera", Outcome: coreexecutor.RouteOutcomeSuccess},
	}
	if routingTraceNamesProvider(events, "ghost") {
		t.Fatal("a failed-only provider was treated as corroborated")
	}
	if !routingTraceNamesProvider(events, "providera") {
		t.Fatal("the serving provider was not corroborated")
	}
}
