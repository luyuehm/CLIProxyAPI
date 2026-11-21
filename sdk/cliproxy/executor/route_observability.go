package executor

import (
	"context"
	"strings"
	"time"
)

// RouteObservabilityReporter captures credential-level routing decisions made
// by the auth conductor for one request: each provider/credential attempt and
// its outcome. The HTTP layer registers an implementation through the request
// context so a debug-flagged request can surface the routing decision without
// other requests paying for the bookkeeping. When no reporter is present in
// the context, the conductor emits no routing events at all.
type RouteObservabilityReporter interface {
	// ReportRouteEvent records one routing attempt. Implementations must be
	// safe for concurrent use across the retry goroutines of one request.
	ReportRouteEvent(event RouteEvent)
}

// RouteOutcome values for RouteEvent.Outcome.
const (
	RouteOutcomeSuccess          = "success"
	RouteOutcomeFailed           = "failed"
	RouteOutcomePreparationError = "preparation_failed"
)

// RouteEvent is one credential-level routing attempt observed by the
// conductor for a single request.
type RouteEvent struct {
	// Provider is the provider key of the executor that served the attempt.
	Provider string
	// AuthIndex is the stable credential index chosen for the attempt.
	AuthIndex string
	// Model is the upstream model identifier used for the attempt.
	Model string
	// Outcome is the terminal outcome of the attempt: one of the
	// RouteOutcome* constants.
	Outcome string
	// Status is the upstream HTTP status observed for the failed attempt
	// (0 when unknown). Empty semantics: success outcomes carry 0.
	Status int
	// Code is the short machine-readable failure code (e.g. "upstream_503").
	// Empty for success outcomes.
	Code string
	// Error is a sanitized, client-safe one-line description of the failure.
	// It never contains credentials; empty for success outcomes.
	Error string
	// Timestamp is the wall-clock time the outcome was observed.
	Timestamp time.Time
}

type routeObservabilityContextKey struct{}

// WithRouteObservability returns a context carrying a per-request routing
// observer. The observer is read by the auth conductor when an execution
// outcome is recorded; requests without one pay no reporting cost.
func WithRouteObservability(ctx context.Context, reporter RouteObservabilityReporter) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if reporter == nil {
		return ctx
	}
	return context.WithValue(ctx, routeObservabilityContextKey{}, reporter)
}

// RouteObservabilityFromContext returns the per-request routing observer
// registered by the HTTP layer, or nil when routing observability is off.
func RouteObservabilityFromContext(ctx context.Context) RouteObservabilityReporter {
	if ctx == nil {
		return nil
	}
	reporter, _ := ctx.Value(routeObservabilityContextKey{}).(RouteObservabilityReporter)
	return reporter
}

// SanitizeRouteEventError trims an error description for inclusion in a
// client-visible routing report. It caps the result so a verbose upstream body
// cannot bloat the response.
func SanitizeRouteEventError(message string, limit int) string {
	trimmed := strings.TrimSpace(message)
	if limit > 0 && len(trimmed) > limit {
		trimmed = trimmed[:limit] + "..."
	}
	return trimmed
}
