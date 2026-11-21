package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// routeEventErrorLimit caps failure descriptions surfaced to clients.
const routeEventErrorLimit = 512

// reportRouteEvent records one credential-level routing outcome for a request.
// It is a no-op when no per-request observer is registered in the context, so
// the default path pays no reporting cost.
func reportRouteEvent(ctx context.Context, provider string, auth *Auth, model, outcome string, resultErr *Error) {
	reporter := cliproxyexecutor.RouteObservabilityFromContext(ctx)
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
		if event.Code == "" && event.Status > 0 {
			event.Code = fmt.Sprintf("upstream_%d", event.Status)
		}
		event.Error = sanitizedRouteEventError(resultErr)
	}
	reporter.ReportRouteEvent(event)
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
