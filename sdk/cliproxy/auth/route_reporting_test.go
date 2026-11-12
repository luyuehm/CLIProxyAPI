package auth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type recordingRouteReporter struct {
	mu     sync.Mutex
	events []cliproxyexecutor.RouteEvent
}

func (r *recordingRouteReporter) ReportEvent(event cliproxyexecutor.RouteEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *recordingRouteReporter) snapshot() []cliproxyexecutor.RouteEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]cliproxyexecutor.RouteEvent(nil), r.events...)
}

func TestRouteReporterForOptions(t *testing.T) {
	if got := routeReporterForOptions(cliproxyexecutor.Options{}); got != nil {
		t.Fatalf("nil metadata must yield a nil reporter, got %v", got)
	}
	if got := routeReporterForOptions(cliproxyexecutor.Options{Metadata: map[string]any{"other": 1}}); got != nil {
		t.Fatalf("metadata without the key must yield a nil reporter, got %v", got)
	}
	reporter := &recordingRouteReporter{}
	opts := cliproxyexecutor.Options{Metadata: map[string]any{
		cliproxyexecutor.RouteInfoReporterMetadataKey: reporter,
	}}
	if got := routeReporterForOptions(opts); got != reporter {
		t.Fatalf("reporter not returned from metadata: %v", got)
	}
}

func TestReportRouteEventNoReporterIsInert(t *testing.T) {
	// Must not panic when no reporter is registered (the default path).
	reportRouteEvent(cliproxyexecutor.Options{}, "p", &Auth{ID: "a", Index: "1"}, "m", cliproxyexecutor.RouteOutcomeFailed, &Error{Message: "boom"})
}

func TestReportRouteEventCapturesOutcome(t *testing.T) {
	reporter := &recordingRouteReporter{}
	opts := cliproxyexecutor.Options{Metadata: map[string]any{
		cliproxyexecutor.RouteInfoReporterMetadataKey: reporter,
	}}
	auth := &Auth{ID: "auth-a", Index: "7"}
	reportRouteEvent(opts, "provider-x", auth, "gpt-5.1", cliproxyexecutor.RouteOutcomeFailed, &Error{Code: "upstream_503", Message: "upstream overloaded", HTTPStatus: http.StatusServiceUnavailable})

	events := reporter.snapshot()
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	event := events[0]
	if event.Provider != "provider-x" || event.AuthIndex != "7" || event.Model != "gpt-5.1" {
		t.Fatalf("unexpected event identity: %+v", event)
	}
	if event.Outcome != cliproxyexecutor.RouteOutcomeFailed {
		t.Fatalf("outcome = %q", event.Outcome)
	}
	if event.Status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", event.Status)
	}
	if event.Code != "upstream_503" {
		t.Fatalf("code = %q", event.Code)
	}
	if event.Error == "" {
		t.Fatal("failure events must carry a description")
	}
	if event.Timestamp.IsZero() {
		t.Fatal("timestamp must be set")
	}
}

func TestReportRouteEventSuccessHasNoError(t *testing.T) {
	reporter := &recordingRouteReporter{}
	opts := cliproxyexecutor.Options{Metadata: map[string]any{
		cliproxyexecutor.RouteInfoReporterMetadataKey: reporter,
	}}
	reportRouteEvent(opts, "provider-x", &Auth{ID: "auth-a", Index: "3"}, "m", cliproxyexecutor.RouteOutcomeSuccess, nil)

	events := reporter.snapshot()
	if len(events) != 1 || events[0].Outcome != cliproxyexecutor.RouteOutcomeSuccess {
		t.Fatalf("unexpected events: %+v", events)
	}
	if events[0].Error != "" || events[0].Status != 0 || events[0].Code != "" {
		t.Fatalf("success event must be empty: %+v", events[0])
	}
}

func TestSanitizedRouteEventErrorRedacts(t *testing.T) {
	redacted := sanitizedRouteEventError(&Error{Message: `Authorization: Bearer sk-secret-token-123 failed`})
	if redacted == "" {
		t.Fatal("expected a description")
	}
	if containsSubstring(redacted, "sk-secret-token-123") {
		t.Fatalf("credential material leaked into the client-visible message: %q", redacted)
	}
}

func TestSanitizedRouteEventErrorCapsLength(t *testing.T) {
	long := make([]byte, routeEventErrorLimit*2)
	for i := range long {
		long[i] = 'a'
	}
	got := sanitizedRouteEventError(&Error{Message: string(long)})
	if len(got) > routeEventErrorLimit+len("...") {
		t.Fatalf("message not capped: len=%d", len(got))
	}
}

func TestSanitizedRouteEventErrorNil(t *testing.T) {
	if got := sanitizedRouteEventError(nil); got != "" {
		t.Fatalf("nil error should produce an empty description, got %q", got)
	}
}

// Ensure the error type used by tests stays compatible with errors.Is/As.
var _ = errors.Is
var _ = context.Background

func containsSubstring(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
