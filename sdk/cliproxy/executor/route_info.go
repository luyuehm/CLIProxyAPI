package executor

import (
	"strings"
	"time"
)

// RouteInfoReporter captures credential-level routing decisions made by the
// auth conductor for one request: each provider/credential attempt and its
// outcome. Handlers register an implementation through
// RouteInfoReporterMetadataKey so a debug-flagged request can surface the
// routing decision without other requests paying for the bookkeeping. When the
// metadata key is absent or nil, the conductor emits no routing events at all.
type RouteInfoReporter interface {
	// ReportEvent records one routing attempt. Implementations must be safe
	// for concurrent use across the retry goroutines of one request.
	ReportEvent(event RouteEvent)
}

// RouteInfoReporterMetadataKey is the executor.Options.Metadata key under which
// a RouteInfoReporter is carried for the duration of one request.
const RouteInfoReporterMetadataKey = "route_info_reporter"

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

// SanitizeRouteEventError trims an error description for inclusion in a
// client-visible routing report. It redacts credential material the same way
// the conductor's log diagnostics do and caps the result so a verbose upstream
// body cannot bloat the response.
func SanitizeRouteEventError(message string, limit int) string {
	trimmed := strings.TrimSpace(message)
	if limit > 0 && len(trimmed) > limit {
		trimmed = trimmed[:limit] + "..."
	}
	return trimmed
}
