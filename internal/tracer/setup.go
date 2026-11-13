package tracer

import (
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

// Environment variables controlling the auto-trace channel (RIC-2508).
// All are optional; the channel is off by default so an unconfigured build
// behaves exactly as before. The conventions mirror the content-filter
// audit channel: KEEPER base URL resolution and the shared management key.
const (
	// EnvEnabled forces the channel on/off. "1"|"true"|"yes"|"on" enables.
	// Unset means disabled.
	EnvEnabled = "CPA_TRACE_ENABLED"
	// EnvLangsmithTracing is the LangSmith-compatible enable alias. An
	// application that already sets LANGSMITH_TRACING=true to turn on SDK
	// tracing can rely on the same switch to light up gateway auto-tracing
	// without a second env var.
	EnvLangsmithTracing = "LANGSMITH_TRACING"
	// EnvKeeperURL is the KEEPER control-plane base URL for trace ingest.
	// When unset it is derived from CPA_BASE_URL with the port rewritten to
	// 4320, matching the sibling KEEPER channels.
	EnvKeeperURL = "CPA_TRACE_KEEPER_URL"
	// EnvKeeperKey is the shared X-CPA-Management-Key for the ingest POST.
	// When unset it falls back to the key the content-filter channel shares
	// with KEEPER, so deployments already wired for that channel need no
	// new secret.
	EnvKeeperKey = "CPA_TRACE_KEEPER_KEY"
	// EnvIngestToken is a dedicated KEEPER trace-ingest token sent as
	// X-Trace-Ingest-Token. When KEEPER has TRACE_INGEST_TOKEN configured
	// its ingest requires BOTH the standard auth (management key) and this
	// token, so a deployment that enables the token must set it here too.
	// Unset when KEEPER runs ingest on management-key auth only.
	EnvIngestToken = "CPA_TRACE_INGEST_TOKEN"
	// EnvManagementKeyFallback reuses the shared management key from the
	// content-filter channel.
	EnvManagementKeyFallback = "CPA_CONTENT_FILTER_AUDIT_KEEPER_KEY"
	// EnvCPAURL is KEEPER's view of the CPA base URL; the dev-time
	// fallback source for EnvKeeperURL.
	EnvCPAURL = "CPA_BASE_URL"
	// EnvQueueSize overrides the in-flight report queue depth.
	EnvQueueSize = "CPA_TRACE_QUEUE_SIZE"
	// EnvRetryBuffer overrides the retry-buffer depth.
	EnvRetryBuffer = "CPA_TRACE_RETRY_BUFFER"
)

// logger is the package-level logger.
var logger = log.WithField("component", "tracer")

// Runtime bundles the live reporter and the plugin handle so the builder
// can start, stop and inspect the channel.
type Runtime struct {
	reporter *Reporter
	plugin   string
}

// Start launches the reporter worker. No-op when the channel is disabled.
func (r *Runtime) Start() {
	if r == nil || r.reporter == nil {
		return
	}
	r.reporter.Start()
}

// Stop flushes and stops the reporter. Idempotent.
func (r *Runtime) Stop() {
	if r == nil || r.reporter == nil {
		return
	}
	r.reporter.Stop()
}

// Reporter exposes the reporter for status handlers and tests.
func (r *Runtime) Reporter() *Reporter {
	if r == nil {
		return nil
	}
	return r.reporter
}

// Build wires the channel from the environment. It registers the reporter
// as a named usage plugin (replacing any earlier one under the same name)
// and reports whether the channel is active so the caller can mount the
// trace-adopt middleware. A nil runtime and active=false mean the channel
// is disabled or unconfigured and the gateway runs exactly as before.
//
// The middleware is mounted by the caller (sdk/cliproxy/builder.go) via
// api.WithMiddleware, not inside this package: internal/api sits at the
// top of the import graph, so depending on it here would cycle through
// the handler layer.
func Build() (*Runtime, bool) {
	if !enabledFromEnv() {
		return nil, false
	}
	keeperURL := keeperURLFromEnv()
	if keeperURL == "" {
		logger.Warn("auto-trace enabled but no KEEPER source: set " + EnvKeeperURL + " (or " + EnvCPAURL + ") to enable reporting")
		return nil, false
	}
	reporter := NewReporter(ReporterConfig{
		KeeperURL:     keeperURL,
		ManagementKey: keeperKeyFromEnv(),
		IngestToken:   strings.TrimSpace(os.Getenv(EnvIngestToken)),
		QueueSize:     intFromEnv(EnvQueueSize),
		RetryBuffer:   intFromEnv(EnvRetryBuffer),
	})
	coreusage.RegisterNamedPlugin("tracer", reporter)
	logger.Infof("auto-trace channel built: ingest=%s (set %s=true and a KEEPER source to report)", keeperURL, EnvEnabled)
	return &Runtime{reporter: reporter}, true
}

// Middleware builds the per-request trace-adopt middleware. It is mounted
// on the gin engine through the WithMiddleware option by the SDK builder.
func Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		trace := AdoptFromRequest(c.Request)
		if trace == nil {
			c.Next()
			return
		}
		// Seed the request context: the handler layer (GetContextWithCancel)
		// lifts it into the execution context, which is where the usage
		// records — and therefore the trace reports — are built.
		c.Request = c.Request.WithContext(WithTraceContext(c.Request.Context(), trace))
		// Echo the trace context back so downstream clients see this
		// node's span in the chain. An empty tracestate (passthrough-only)
		// is not set, which would write a blank header.
		if vendor := trace.Vendor; vendor != "" {
			c.Header(tracestateHeader, vendor)
		}
		if tp := FormatTraceparent(trace, DeterministicSpanID(trace.TraceID, requestIDFrom(c))); tp != "" {
			c.Header(traceparentHeader, tp)
		}
		c.Next()
	}
}

// AdoptFromRequest builds the TraceContext for an incoming request: it
// adopts the W3C traceparent when present and valid (an invalid value is
// rejected — silently ignoring it would make "I sent a context but it
// shows uncorrelated" hard to debug), otherwise generates a fresh trace
// so gateway calls always produce a span.
func AdoptFromRequest(r *http.Request) *TraceContext {
	if r == nil {
		return GenerateTraceContext()
	}
	raw := strings.TrimSpace(r.Header.Get(traceparentHeader))
	if raw == "" {
		return GenerateTraceContext()
	}
	trace, err := ParseTraceparent(raw)
	if err != nil {
		logger.Warnf("tracer: rejecting invalid traceparent %q: %v", raw, err)
		return GenerateTraceContext()
	}
	trace.Vendor = strings.TrimSpace(r.Header.Get(tracestateHeader))
	return trace
}

// requestIDFrom resolves the per-request id the middleware echoes in the
// traceparent header. The request-id middleware (GinLogrusLogger) runs
// before this one on AI API paths; off those paths the id is empty and
// the generated span id is used instead.
func requestIDFrom(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	if id := internallogging.GetRequestID(c.Request.Context()); id != "" {
		return id
	}
	return internallogging.GetGinRequestID(c)
}

// enabledFromEnv resolves the enable switch. An explicit value on the
// canonical switch wins; the LangSmith alias enables when the operator
// has not spoken.
func enabledFromEnv() bool {
	if value := strings.ToLower(strings.TrimSpace(os.Getenv(EnvEnabled))); value != "" {
		switch value {
		case "1", "true", "yes", "on":
			return true
		default:
			return false
		}
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvLangsmithTracing))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// keeperURLFromEnv resolves the KEEPER ingest base URL: the explicit env
// wins, otherwise the dev fallback derives it from CPA_BASE_URL with port
// 4320.
func keeperURLFromEnv() string {
	if url := strings.TrimSpace(os.Getenv(EnvKeeperURL)); url != "" {
		return url
	}
	base := strings.TrimSpace(os.Getenv(EnvCPAURL))
	if base == "" {
		return ""
	}
	trimmed := strings.TrimRight(base, "/")
	if idx := strings.LastIndex(trimmed, ":"); idx > 0 {
		return trimmed[:idx] + ":4320"
	}
	return trimmed + ":4320"
}

// keeperKeyFromEnv resolves the shared management key: the explicit env
// wins, then the key all KEEPER-facing CPA channels already share.
func keeperKeyFromEnv() string {
	if key := strings.TrimSpace(os.Getenv(EnvKeeperKey)); key != "" {
		return key
	}
	return strings.TrimSpace(os.Getenv(EnvManagementKeyFallback))
}

// intFromEnv reads a positive integer env override; unset or invalid
// returns 0 and the caller keeps its default.
func intFromEnv(name string) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return 0
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return 0
	}
	return value
}
