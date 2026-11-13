package tracer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

// IngestRequest is the JSON payload CPA POSTs to KEEPER's trace ingest
// endpoint (POST /api/v1/traces/ingest). It mirrors the KEEPER-side
// contract: a trace head plus one span per gateway model call.
type IngestRequest struct {
	TraceID   string `json:"trace_id"`
	RequestID string `json:"request_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Kind      string `json:"kind,omitempty"`
	StartedAt string `json:"started_at,omitempty"`
	// Traceparent is the W3C carrier KEEPER's ingest reads when the HTTP
	// header is absent (offline re-report). Its trace-id is authoritative
	// over TraceID on the KEEPER side.
	Traceparent string `json:"traceparent,omitempty"`
	Spans       []Span `json:"spans"`
}

// Span is one trace span in the ingest payload.
type Span struct {
	ID               string         `json:"id"`
	ParentSpanID     string         `json:"parent_span_id,omitempty"`
	Name             string         `json:"name,omitempty"`
	Kind             string         `json:"kind"`
	Status           string         `json:"status"`
	Model            string         `json:"model,omitempty"`
	Provider         string         `json:"provider,omitempty"`
	StartOffsetUS    int64          `json:"start_offset_us"`
	DurationUS       int64          `json:"duration_us"`
	InputTokens      int64          `json:"input_tokens"`
	OutputTokens     int64          `json:"output_tokens"`
	CacheReadTokens  int64          `json:"cache_read_tokens"`
	CacheWriteTokens int64          `json:"cache_write_tokens"`
	ErrorMessage     string         `json:"error_message,omitempty"`
	Attributes       map[string]any `json:"attributes,omitempty"`
	StartedAt        string         `json:"started_at,omitempty"`
}

const (
	// maxErrorMessage caps the error_message column on the KEEPER side.
	maxErrorMessage = 512
)

// DeterministicSpanID derives a stable W3C-width span id from the trace id
// and the request id. A gateway model call always reports the same span
// identity, so a retried or duplicate report upserts over itself on the
// KEEPER side instead of creating a second span for one call.
func DeterministicSpanID(traceID, requestID string) string {
	sum := sha256.Sum256([]byte(traceID + "|" + requestID))
	return hex.EncodeToString(sum[:8])
}

// BuildIngestRequest converts one usage record (a completed gateway model
// call) into the KEEPER trace ingest payload. ctx must carry the per-request
// TraceContext (seeded by AdoptFromRequest at the handler boundary); when
// it is absent a fresh trace is generated so a record is never dropped.
//
// It returns ok=false only when the record is empty enough that there is
// nothing to report (no model, no request time) — degenerate records must
// not pollute the trace list.
func BuildIngestRequest(ctx context.Context, record coreusage.Record) (IngestRequest, bool) {
	model := strings.TrimSpace(record.Model)
	alias := strings.TrimSpace(record.Alias)
	if alias == "" {
		alias = model
	}
	if model == "" && alias == "" {
		return IngestRequest{}, false
	}
	if record.RequestedAt.IsZero() {
		record.RequestedAt = time.Now()
	}

	trace := TraceContextFromContext(ctx)
	if trace == nil || trace.TraceID == "" {
		trace = GenerateTraceContext()
	}

	requestID := internallogging.GetRequestID(ctx)
	spanID := DeterministicSpanID(trace.TraceID, requestID)
	if requestID == "" {
		// No request id on this context (embedded-SDK usage without the
		// HTTP layer): fall back to a random id so the span is unique.
		spanID = GenerateSpanID()
	}

	sessionID := strings.TrimSpace(record.SessionID)
	name := "llm:" + alias

	span := Span{
		ID:        spanID,
		Name:      name,
		Kind:      "llm",
		Status:    "ok",
		Model:     model,
		Provider:  strings.TrimSpace(record.Provider),
		StartedAt: record.RequestedAt.Format(time.RFC3339Nano),
	}
	if trace.ParentSpanID != "" {
		span.ParentSpanID = trace.ParentSpanID
	}
	detail := record.Detail
	span.InputTokens = detail.InputTokens
	span.OutputTokens = detail.OutputTokens
	span.CacheReadTokens = detail.CacheReadTokens
	span.CacheWriteTokens = detail.CacheCreationTokens
	if latency := record.Latency; latency > 0 {
		span.DurationUS = latency.Nanoseconds() / 1000
	}
	if record.Failed {
		span.Status = "error"
		span.ErrorMessage = trimErrorMessage(record.Fail.Body)
	}

	// Attribute keys are chosen to survive KEEPER's sensitive-key filter:
	// none contains a sensitive word (authorization/token/key/secret/...).
	// Values are metadata only; no request/response bodies, no credentials.
	attributes := map[string]any{}
	setAttribute := func(key, value string) {
		if value != "" {
			attributes[key] = value
		}
	}
	setAttribute("provider", record.Provider)
	setAttribute("executor", record.ExecutorType)
	setAttribute("auth_index", record.AuthIndex)
	setAttribute("auth_type", record.AuthType)
	setAttribute("source", record.Source)
	setAttribute("service_tier", record.ServiceTier)
	setAttribute("reasoning_effort", record.ReasoningEffort)
	setAttribute("session_id", record.SessionID)
	setAttribute("parent_session_id", record.ParentSessionID)
	setAttribute("endpoint", internallogging.GetEndpoint(ctx))
	if record.Stream {
		attributes["stream"] = true
	}
	span.Attributes = attributes

	req := IngestRequest{
		TraceID:   trace.TraceID,
		RequestID: requestID,
		SessionID: sessionID,
		Name:      name,
		Kind:      "request",
		StartedAt: record.RequestedAt.Format(time.RFC3339Nano),
		Spans:     []Span{span},
	}
	if trace.ParentSpanID != "" {
		req.Traceparent = FormatTraceparent(trace, trace.ParentSpanID)
	}
	return req, true
}

func trimErrorMessage(body string) string {
	trimmed := strings.TrimSpace(body)
	if len(trimmed) > maxErrorMessage {
		return trimmed[:maxErrorMessage]
	}
	return trimmed
}

// Encode renders the payload for HTTP transmission.
func (r IngestRequest) Encode() ([]byte, error) {
	return json.Marshal(r)
}
