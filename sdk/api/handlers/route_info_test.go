package handlers

import (
	"net/url"
	"testing"

	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
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
	collector.ReportEvent(coreexecutor.RouteEvent{Provider: "openai", AuthIndex: "1", Model: "gpt-5.1", Outcome: coreexecutor.RouteOutcomeSuccess})

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
	collector.ReportEvent(coreexecutor.RouteEvent{Provider: "primary", AuthIndex: "2", Model: "m", Outcome: coreexecutor.RouteOutcomeFailed, Status: 503, Error: "upstream 503"})
	collector.ReportEvent(coreexecutor.RouteEvent{Provider: "backup", AuthIndex: "9", Model: "m", Outcome: coreexecutor.RouteOutcomeSuccess})

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
	collector.ReportEvent(coreexecutor.RouteEvent{Provider: "openai", AuthIndex: "1", Outcome: coreexecutor.RouteOutcomeSuccess})

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
	collector.ReportEvent(coreexecutor.RouteEvent{Provider: "openai", Outcome: coreexecutor.RouteOutcomeSuccess})

	body := []byte("not-json")
	injected := collector.InjectIntoJSON(nil, body)
	if string(injected) != string(body) {
		t.Fatalf("invalid body should be returned unchanged, got %q", injected)
	}
}

func TestNilCollectorReportingIsInert(t *testing.T) {
	var collector *RouteInfoCollector
	collector.ReportEvent(coreexecutor.RouteEvent{Provider: "x", Outcome: coreexecutor.RouteOutcomeSuccess})
	if got := collector.JSON(nil); got != "" {
		t.Fatalf("nil collector JSON = %q, want empty", got)
	}
	if got := collector.InjectIntoJSON(nil, []byte(`{"a":1}`)); string(got) != `{"a":1}` {
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
