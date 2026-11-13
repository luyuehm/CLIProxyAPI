package auth

import (
	"errors"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// routeEventErrorLimit caps failure descriptions surfaced to clients.
const routeEventErrorLimit = 512

// routeReporterForOptions returns the optional per-request routing reporter
// registered by the HTTP layer, or nil when routing reporting is off.
func routeReporterForOptions(opts cliproxyexecutor.Options) cliproxyexecutor.RouteInfoReporter {
	if len(opts.Metadata) == 0 {
		return nil
	}
	reporter, _ := opts.Metadata[cliproxyexecutor.RouteInfoReporterMetadataKey].(cliproxyexecutor.RouteInfoReporter)
	return reporter
}

// reportRouteEvent records one credential-level routing outcome for a request.
// It is a no-op when no reporter is registered, so the default (debug off)
// path pays no cost.
func reportRouteEvent(opts cliproxyexecutor.Options, provider string, auth *Auth, model, outcome string, resultErr *Error) {
	reporter := routeReporterForOptions(opts)
	if reporter == nil || auth == nil {
		return
	}
	event := cliproxyexecutor.RouteEvent{
		Provider:  provider,
		AuthIndex: auth.EnsureIndex(),
		Model:     model,
		Outcome:   outcome,
		Timestamp: time.Now(),
	}
	if resultErr != nil {
		event.Status = resultErr.HTTPStatus
		event.Code = resultErr.Code
		event.Error = sanitizedRouteEventError(resultErr)
	}
	reporter.ReportEvent(event)
}

// sanitizedRouteEventError produces a client-safe one-line description of a
// routing failure. It honors the error's own LogDiagnostic marker when
// present and redacts credential material the same way the conductor's log
// diagnostics do, then caps the result.
func sanitizedRouteEventError(err *Error) string {
	if err == nil {
		return ""
	}
	diagnostic := err.Message
	if diagnostic == "" {
		diagnostic = err.Error()
	}
	type logDiagnosticError interface {
		LogDiagnostic() string
	}
	var diagnosticErr logDiagnosticError
	if errors.As(err, &diagnosticErr) && diagnosticErr != nil {
		if marked := strings.TrimSpace(diagnosticErr.LogDiagnostic()); marked != "" {
			diagnostic = marked
		}
	}
	return cliproxyexecutor.SanitizeRouteEventError(logging.SafeDiagnosticForLog(diagnostic), routeEventErrorLimit)
}
