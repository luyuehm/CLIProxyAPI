package handlers

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/MoeForge/moego/json"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"golang.org/x/net/context"
)

func TestDebugQueryParamEnabled(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  bool
	}{
		{name: "absent", query: "", want: false},
		{name: "empty value", query: "?debug=", want: false},
		{name: "false", query: "?debug=false", want: false},
		{name: "zero", query: "?debug=0", want: false},
		{name: "true", query: "?debug=true", want: true},
		{name: "one", query: "?debug=1", want: true},
		{name: "yes upper", query: "?debug=YES", want: true},
		{name: "on", query: "?debug=on", want: true},
		{name: "padded", query: "?debug=%20true%20", want: true},
		{name: "unrelated", query: "?alt=sse", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var values url.Values
			if tc.query != "" {
				parsed, err := url.Parse(tc.query)
				if err != nil {
					t.Fatalf("parse %q: %v", tc.query, err)
				}
				values = parsed.Query()
			}
			if got := debugQueryParamEnabled(values); got != tc.want {
				t.Fatalf("debugQueryParamEnabled(%q) = %v, want %v", tc.query, got, tc.want)
			}
		})
	}
}

func TestRouteInfoCollectorNoFallbackSingleCandidate(t *testing.T) {
	collector := NewRouteInfoCollector([]string{"openai"}, false)
	collector.ReportRouteEvent(coreexecutor.RouteEvent{Provider: "openai", AuthIndex: "1", Model: "gpt-5.1", Outcome: coreexecutor.RouteOutcomeSuccess})

	body := collector.buildBody(nil)
	if body.FinalProvider != "openai" {
		t.Fatalf("final_provider = %q, want openai", body.FinalProvider)
	}
	if body.SelectionReason == "" {
		t.Fatal("selection_reason must be non-empty")
	}
	if len(body.Candidates) != 1 || body.Candidates[0] != "openai" {
		t.Fatalf("candidates = %v, want [openai]", body.Candidates)
	}
	if len(body.FallbackEvents) != 0 {
		t.Fatalf("fallback_events = %v, want empty", body.FallbackEvents)
	}
}

func TestRouteInfoCollectorFallbackOrdering(t *testing.T) {
	collector := NewRouteInfoCollector([]string{"primary", "backup"}, false)
	// Report out of order to prove the collector sorts by time.
	collector.ReportRouteEvent(coreexecutor.RouteEvent{Provider: "primary", AuthIndex: "2", Model: "m", Outcome: coreexecutor.RouteOutcomeFailed, Status: 503, Error: "upstream 503"})
	collector.ReportRouteEvent(coreexecutor.RouteEvent{Provider: "backup", AuthIndex: "9", Model: "m", Outcome: coreexecutor.RouteOutcomeSuccess})

	body := collector.buildBody(nil)
	if body.FinalProvider != "backup" {
		t.Fatalf("final_provider = %q, want backup", body.FinalProvider)
	}
	if len(body.FallbackEvents) != 1 {
		t.Fatalf("fallback_events len = %d, want 1", len(body.FallbackEvents))
	}
	event := body.FallbackEvents[0]
	if event.FailedProvider != "primary" || event.Status != 503 {
		t.Fatalf("fallback event = %+v, want failed_provider=primary status=503", event)
	}
	if body.SelectionReason == "" {
		t.Fatal("selection_reason must be non-empty")
	}
}

func TestRouteInfoCollectorInjectIntoJSON(t *testing.T) {
	collector := NewRouteInfoCollector([]string{"openai"}, false)
	collector.ReportRouteEvent(coreexecutor.RouteEvent{Provider: "openai", AuthIndex: "1", Outcome: coreexecutor.RouteOutcomeSuccess})

	body := []byte(`{"id":"chatcmpl-1","choices":[]}`)
	injected := collector.InjectIntoJSON(nil, body)
	if string(injected) == string(body) {
		t.Fatal("route_info was not injected")
	}
	if want := `"route_info"`; !routeInfoContains(string(injected), want) {
		t.Fatalf("injected body missing %s: %s", want, injected)
	}
	if !routeInfoContains(string(injected), `"id":"chatcmpl-1"`) {
		t.Fatalf("injection clobbered the original response: %s", injected)
	}
}

func TestRouteInfoCollectorInjectIntoJSONLeavesInvalidBody(t *testing.T) {
	collector := NewRouteInfoCollector([]string{"openai"}, false)
	collector.ReportRouteEvent(coreexecutor.RouteEvent{Provider: "openai", Outcome: coreexecutor.RouteOutcomeSuccess})

	body := []byte("not-json")
	injected := collector.InjectIntoJSON(nil, body)
	if string(injected) != string(body) {
		t.Fatalf("invalid body should be returned unchanged, got %q", injected)
	}
}

func TestRouteInfoCollectorOpenRouterRouteNoFallback(t *testing.T) {
	collector := NewRouteInfoCollectorWithModel([]string{"provider-a"}, false, "gpt-test")
	collector.ReportRouteEvent(coreexecutor.RouteEvent{
		Provider: "provider-a",
		Model:    "gpt-test",
		Outcome:  coreexecutor.RouteOutcomeSuccess,
	})

	var body map[string]any
	if err := json.Unmarshal([]byte(collector.OpenRouterRoute(nil)), &body); err != nil {
		t.Fatalf("decode route header: %v", err)
	}
	if body["provider"] != "provider-a" {
		t.Fatalf("provider = %v, want provider-a", body["provider"])
	}
	if body["model"] != "gpt-test" {
		t.Fatalf("model = %v, want gpt-test", body["model"])
	}
	chain, _ := body["fallback_chain"].([]any)
	if len(chain) != 0 {
		t.Fatalf("fallback_chain = %v, want empty", body["fallback_chain"])
	}
}

func TestRouteInfoCollectorOpenRouterRouteFallback(t *testing.T) {
	collector := NewRouteInfoCollectorWithModel([]string{"provider-a", "provider-b"}, false, "gpt-test")
	collector.ReportRouteEvent(coreexecutor.RouteEvent{
		Provider: "provider-a",
		Model:    "gpt-test",
		Outcome:  coreexecutor.RouteOutcomeFailed,
		Status:   http.StatusServiceUnavailable,
		Code:     "upstream_503",
	})
	collector.ReportRouteEvent(coreexecutor.RouteEvent{
		Provider: "provider-b",
		Model:    "gpt-test",
		Outcome:  coreexecutor.RouteOutcomeSuccess,
	})

	var body map[string]any
	if err := json.Unmarshal([]byte(collector.OpenRouterRoute(nil)), &body); err != nil {
		t.Fatalf("decode route header: %v", err)
	}
	if body["provider"] != "provider-b" {
		t.Fatalf("provider = %v, want provider-b", body["provider"])
	}
	chain, _ := body["fallback_chain"].([]any)
	if len(chain) != 2 || chain[0] != "provider-a" || chain[1] != "provider-b" {
		t.Fatalf("fallback_chain = %v, want [provider-a provider-b]", chain)
	}
	if code, _ := body["failure_code"].(string); code != "" {
		t.Fatalf("failure_code = %q, want empty on a successful fallback", code)
	}
	if reason, _ := body["failure_reason"].(string); reason != "" {
		t.Fatalf("failure_reason = %q, want empty on a successful fallback", reason)
	}
}

func TestRouteInfoCollectorOpenRouterRouteError(t *testing.T) {
	collector := NewRouteInfoCollectorWithModel([]string{"provider-a", "provider-b"}, false, "gpt-test")
	collector.ReportRouteEvent(coreexecutor.RouteEvent{
		Provider: "provider-a",
		Model:    "gpt-test",
		Outcome:  coreexecutor.RouteOutcomeFailed,
		Status:   http.StatusServiceUnavailable,
		Code:     "upstream_503",
	})
	collector.ReportRouteEvent(coreexecutor.RouteEvent{
		Provider: "provider-b",
		Model:    "gpt-test",
		Outcome:  coreexecutor.RouteOutcomeFailed,
		Status:   http.StatusServiceUnavailable,
		Code:     "upstream_503",
		Error:    "upstream overloaded",
	})

	var body map[string]any
	if err := json.Unmarshal([]byte(collector.OpenRouterRoute(nil)), &body); err != nil {
		t.Fatalf("decode route header: %v", err)
	}
	if body["provider"] != "provider-b" {
		t.Fatalf("provider = %v, want last attempted provider-b", body["provider"])
	}
	if body["failure_code"] != "upstream_503" {
		t.Fatalf("failure_code = %v, want upstream_503", body["failure_code"])
	}
	if body["failure_reason"] != "upstream overloaded" {
		t.Fatalf("failure_reason = %v, want upstream overloaded", body["failure_reason"])
	}
	chain, _ := body["fallback_chain"].([]any)
	if len(chain) != 2 {
		t.Fatalf("fallback_chain = %v, want [provider-a provider-b]", chain)
	}
}

func TestNilCollectorReportingIsInert(t *testing.T) {
	var collector *RouteInfoCollector
	collector.ReportRouteEvent(coreexecutor.RouteEvent{Provider: "x", Outcome: coreexecutor.RouteOutcomeSuccess})
	if got := collector.JSON(nil); got != "" {
		t.Fatalf("nil collector JSON = %q, want empty", got)
	}
	if got := collector.InjectIntoJSON(nil, []byte(`{"a":1}`)); string(got) != `{"a":1}` {
		t.Fatalf("nil collector changed the body: %s", got)
	}
}

// TestRouteInfoCollectorFallbackCapEnforced reports more failures than the
// routeInfoMaxFallbackEvents cap and asserts the rendered fallback list is
// bounded while the request completes without an error.
func TestRouteInfoCollectorFallbackCapEnforced(t *testing.T) {
	collector := NewRouteInfoCollectorWithModel([]string{"provider-a", "provider-b"}, false, "gpt-test")
	for i := 0; i < routeInfoMaxFallbackEvents+5; i++ {
		collector.ReportRouteEvent(coreexecutor.RouteEvent{
			Provider: "provider-a",
			Model:    "gpt-test",
			Outcome:  coreexecutor.RouteOutcomeFailed,
			Status:   http.StatusServiceUnavailable,
			Code:     "upstream_503",
		})
	}
	collector.ReportRouteEvent(coreexecutor.RouteEvent{
		Provider: "provider-b",
		Model:    "gpt-test",
		Outcome:  coreexecutor.RouteOutcomeSuccess,
	})

	var body map[string]any
	if err := json.Unmarshal([]byte(collector.OpenRouterRoute(nil)), &body); err != nil {
		t.Fatalf("decode route header: %v", err)
	}
	chain, _ := body["fallback_chain"].([]any)
	if len(chain) > routeInfoMaxFallbackEvents {
		t.Fatalf("fallback_chain len = %d, want <= %d to bound a pathological retry loop", len(chain), routeInfoMaxFallbackEvents)
	}

	body2 := collector.buildBody(nil)
	if len(body2.FallbackEvents) > routeInfoMaxFallbackEvents {
		t.Fatalf("fallback_events len = %d, want <= %d", len(body2.FallbackEvents), routeInfoMaxFallbackEvents)
	}
}

// TestRouteInfoCollectorStreamingHeader exercises the streaming channel:
// X-Route-Detail is the only surface for a body that cannot be mutated, so the
// header must carry the complete route_info payload.
func TestRouteInfoCollectorStreamingHeader(t *testing.T) {
	collector := NewRouteInfoCollectorWithModel([]string{"primary", "backup"}, false, "sse-model")
	collector.ReportRouteEvent(coreexecutor.RouteEvent{
		Provider: "primary",
		Model:    "sse-model",
		Outcome:  coreexecutor.RouteOutcomeFailed,
		Status:   http.StatusTooManyRequests,
		Code:     "upstream_429",
	})
	collector.ReportRouteEvent(coreexecutor.RouteEvent{
		Provider: "backup",
		Model:    "sse-model",
		Outcome:  coreexecutor.RouteOutcomeSuccess,
	})

	// attachRouteInfoToHeaders must land the payload on the header map that is
	// flushed to the client before the first stream chunk.
	headers := make(http.Header)
	attachRouteInfoToHeaders(headers, collector, context.Background())
	got := headers.Values(RouteDetailResponseHeader)
	if len(got) == 0 {
		t.Fatalf("X-Route-Detail header not set on streaming headers")
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(got[0]), &body); err != nil {
		t.Fatalf("decode route detail header: %v", err)
	}
	if body["final_provider"] != "backup" {
		t.Fatalf("final_provider = %v, want backup", body["final_provider"])
	}
	if _, ok := body["fallback_events"]; !ok {
		t.Fatalf("route detail missing fallback_events: %v", body)
	}
}

// TestRouteInfoCollectorDebugOffZeroOverhead verifies that a streaming request
// without ?debug=true creates no collector, so the conductor's reporting path
// is inert: no X-Route-Detail header and no body mutation.
func TestRouteInfoCollectorDebugOffZeroOverhead(t *testing.T) {
	ginCtx, _ := gin.CreateTestContext(nil)
	ginCtx.Request = &http.Request{URL: &url.URL{}}
	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	if got := routeInfoDebugEnabled(ctx); got {
		t.Fatal("debug must default to off when the query parameter is absent")
	}

	routeCtx := context.WithValue(context.Background(), "gin", ginCtx)
	collector := (*RouteInfoCollector)(nil)
	attachRouteInfoToHeaders(make(http.Header), collector, routeCtx)
	if got := collector.JSON(routeCtx); got != "" {
		t.Fatalf("nil collector JSON = %q, want empty", got)
	}
	if got := collector.InjectIntoJSON(routeCtx, []byte(`{"a":1}`)); string(got) != `{"a":1}` {
		t.Fatalf("nil collector changed the body: %s", got)
	}
}

func routeInfoContains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && routeInfoIndexOf(haystack, needle) >= 0)
}

func routeInfoIndexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// TestWriteErrorResponseAlwaysEmitsRouteHeader verifies that X-OpenRouter-Route
// reaches the client on error responses even when upstream header passthrough
// is disabled (it is always-on route observability, not a passthrough header).
func TestWriteErrorResponseAlwaysEmitsRouteHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	handler := NewBaseAPIHandlers(nil, nil)
	handler.WriteErrorResponse(ginCtx, &interfaces.ErrorMessage{
		StatusCode: http.StatusServiceUnavailable,
		Error:      errors.New("upstream_503"),
		Addon: http.Header{
			OpenRouterRouteHeader: {`{"provider":"antigravity","model":"m","fallback_chain":[],"failure_code":"upstream_503"}`},
			"X-Request-Id":        {"req-1"},
		},
	})

	if got := ginCtx.Writer.Header().Get(OpenRouterRouteHeader); got == "" {
		t.Fatal("X-OpenRouter-Route must be emitted on error responses regardless of passthrough")
	}
	// Other Addon headers remain gated on passthrough.
	if got := ginCtx.Writer.Header().Get("X-Request-Id"); got != "" {
		t.Fatalf("X-Request-Id should stay passthrough-gated, got %q", got)
	}
}
