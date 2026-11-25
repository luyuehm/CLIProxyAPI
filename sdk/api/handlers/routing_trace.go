package handlers

// RIC-3275 (OPENROUT-001): the routing_trace response contract.
//
// The existing observability surface answers "which provider served this
// request" only through response headers (X-OpenRouter-Route / X-OpenRouter-
// Fallback / X-Fallback-Reason) and through the opt-in ?debug=true route_info
// body field. A developer debugging a provider-failover incident needs the
// same facts in the response payload itself, named by a stable contract, so an
// SDK or log pipeline can read them without header parsing and without
// enabling a debug switch.
//
// routing_trace is an additive top-level `metadata.routing_trace` object on
// non-streaming JSON responses. It is rendered from the same per-request
// RouteInfoCollector that feeds the headers and the structured fallback log, so
// the three channels can never disagree (contract: trace data consistency).
//
// Disclosure levels (config `routing-trace.mode`):
//   - "off"     — no routing_trace field.
//   - "summary" — provider_id, model, fallback_used, switch_reason (default).
//   - "full"    — summary plus candidates, decision_path, attempts, latency_ms.
// `routing-trace.full-trace-api-keys` narrows "full" to listed client keys and
// gives every other key the summary variant (the free-tier case), so a caller
// without the detailed entitlement still receives a valid, non-erroring
// response.
//
// The trace never changes a status code, an error body, or a streaming
// response: streaming clients keep the existing always-on headers.

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"golang.org/x/net/context"
)

const (
	// RoutingTraceMetadataField is the top-level response object that carries
	// the trace. Keeping it under `metadata` leaves room for sibling contract
	// fields without inventing one top-level key per feature.
	RoutingTraceMetadataField = "metadata"
	// RoutingTraceField is the trace object name inside metadata.
	RoutingTraceField = "routing_trace"
	// RoutingTraceSchemaVersion pins the payload shape. A breaking shape change
	// must bump it so consumers can branch.
	RoutingTraceSchemaVersion = "1.0"
	// RoutingTraceSwitchReasonNone is the stable value for "no switch
	// happened"; the field is always present in summary/full so consumers can
	// read it unconditionally.
	RoutingTraceSwitchReasonNone = "none"
	// routingTraceAttemptLimit bounds the detailed attempt list so a
	// pathological retry loop cannot bloat the response payload.
	routingTraceAttemptLimit = 16
)

// routingTraceAttempt is one failed routing attempt in the detailed variant.
type routingTraceAttempt struct {
	ProviderID string `json:"provider_id"`
	Model      string `json:"model,omitempty"`
	Status     int    `json:"status,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// routingTraceBody is the routing_trace object. Pointer-free and fully
// specified so omission is explicit (omitempty only on the fields that are
// genuinely absent, never on the AC-mandated ones).
type routingTraceBody struct {
	SchemaVersion string                `json:"schema_version"`
	RequestID     string                `json:"request_id,omitempty"`
	ProviderID    string                `json:"provider_id"`
	Model         string                `json:"model,omitempty"`
	FallbackUsed  bool                  `json:"fallback_used"`
	SwitchReason  string                `json:"switch_reason"`
	DecisionPath  []string              `json:"decision_path,omitempty"`
	Candidates    []string              `json:"candidates,omitempty"`
	Attempts      []routingTraceAttempt `json:"attempts,omitempty"`
	LatencyMS     int64                 `json:"latency_ms,omitempty"`
}

// routingTraceRequested reports whether this execution carries the
// routing_trace contract. It is deliberately narrow: only the client-facing
// chat-completion entry protocols (OpenAI chat, Claude messages, Gemini
// content) opt in, so the Responses API, the Interactions API, video
// endpoints, and internal model callbacks keep their response bodies
// byte-for-byte unchanged.
func routingTraceRequested(entryProtocol string, execOptions modelExecutionOptions) bool {
	if execOptions.InternalSource {
		// A host model callback is not a client response: injecting gateway
		// metadata would change a payload another component parses.
		return false
	}
	switch strings.ToLower(strings.TrimSpace(entryProtocol)) {
	case "openai", "claude", "gemini":
		return true
	default:
		return false
	}
}

// attachRoutingTrace injects metadata.routing_trace into a non-streaming
// response body according to the configured disclosure level. It is a no-op
// (body returned unchanged) when the trace is off, when no provider could be
// attributed, when the body is not a JSON object, or when an existing
// metadata value is not an object (a merge would destroy the upstream field).
func (h *BaseAPIHandler) attachRoutingTrace(ctx context.Context, body []byte, collector *RouteInfoCollector) []byte {
	mode := h.routingTraceMode(ctx)
	if mode == config.RoutingTraceModeOff || collector == nil || len(body) == 0 {
		return body
	}
	trace, ok := collector.buildRoutingTrace(ctx, mode == config.RoutingTraceModeFull)
	if !ok {
		return body
	}
	if !gjson.ValidBytes(body) {
		return body
	}
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return body
	}
	if metadata := root.Get(RoutingTraceMetadataField); metadata.Exists() && !metadata.IsObject() {
		return body
	}
	encoded, errMarshal := json.Marshal(trace)
	if errMarshal != nil {
		return body
	}
	updated, errSet := sjson.SetRawBytes(body, RoutingTraceMetadataField+"."+RoutingTraceField, encoded)
	if errSet != nil || len(updated) == 0 {
		return body
	}
	return updated
}

// routingTraceMode resolves the effective disclosure level for this request:
// the configured mode, narrowed from "full" to "summary" when the inbound key
// is not in the configured full-trace allowlist.
func (h *BaseAPIHandler) routingTraceMode(ctx context.Context) string {
	traceCfg := config.RoutingTraceConfig{}
	if h != nil && h.Cfg != nil {
		traceCfg = h.Cfg.RoutingTrace
	}
	mode := traceCfg.NormalizedMode()
	if mode != config.RoutingTraceModeFull || len(traceCfg.FullTraceAPIKeys) == 0 {
		return mode
	}
	if routingTraceKeyListed(traceCfg.FullTraceAPIKeys, routingTraceCallerKey(ctx)) {
		return mode
	}
	return config.RoutingTraceModeSummary
}

// routingTraceKeyListed reports whether the inbound credential is listed.
// Comparison is verbatim (the credential has already been authenticated); the
// key is never logged, echoed, or included in the response.
func routingTraceKeyListed(keys []string, key string) bool {
	if key == "" {
		return false
	}
	for _, candidate := range keys {
		if strings.TrimSpace(candidate) == key {
			return true
		}
	}
	return false
}

// routingTraceCallerKey extracts the inbound client credential from the three
// header spellings the OpenAI/Anthropic/Gemini-compatible surfaces accept.
func routingTraceCallerKey(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	ginCtx, ok := ctx.Value("gin").(*gin.Context)
	if !ok || ginCtx == nil || ginCtx.Request == nil {
		return ""
	}
	if authorization := strings.TrimSpace(ginCtx.GetHeader("Authorization")); authorization != "" {
		if len(authorization) > 7 && strings.EqualFold(authorization[:7], "bearer ") {
			if token := strings.TrimSpace(authorization[7:]); token != "" {
				return token
			}
		}
	}
	for _, header := range []string{"X-Api-Key", "X-Goog-Api-Key"} {
		if value := strings.TrimSpace(ginCtx.GetHeader(header)); value != "" {
			return value
		}
	}
	return ""
}

// buildRoutingTrace renders the trace from the collected route events. ok is
// false when no provider can be attributed (for example a plugin-executor
// route that reports no credential-level events), in which case no trace is
// attached rather than emitting an empty provider_id.
//
// full controls the detailed fields only; the summary fields are identical in
// both variants so a consumer can read provider_id / switch_reason
// unconditionally.
func (c *RouteInfoCollector) buildRoutingTrace(ctx context.Context, full bool) (routingTraceBody, bool) {
	if c == nil {
		return routingTraceBody{}, false
	}
	c.mu.Lock()
	events := append([]coreexecutor.RouteEvent(nil), c.events...)
	candidates := append([]string(nil), c.candidates...)
	routerRouted := c.routerRouted
	fallbackModel := c.model
	startedAt := c.startedAt
	c.mu.Unlock()

	servingProvider, servingModel := "", ""
	lastProvider, lastModel := "", fallbackModel
	firstFailedProvider, firstFailedModel, firstFailedReason := "", "", ""
	failedAttempts := make([]routingTraceAttempt, 0, len(events))
	for _, event := range events {
		if event.Model != "" {
			lastModel = event.Model
		}
		if event.Provider != "" {
			lastProvider = event.Provider
		}
		if event.Outcome == coreexecutor.RouteOutcomeSuccess {
			if servingProvider == "" {
				servingProvider = event.Provider
				servingModel = event.Model
			}
			continue
		}
		if firstFailedProvider == "" && firstFailedReason == "" {
			firstFailedProvider = event.Provider
			firstFailedModel = event.Model
			firstFailedReason = routingTraceSwitchReason(event)
		}
		if len(failedAttempts) < routingTraceAttemptLimit {
			failedAttempts = append(failedAttempts, routingTraceAttempt{
				ProviderID: event.Provider,
				Model:      event.Model,
				Status:     event.Status,
				Reason:     routingTraceSwitchReason(event),
			})
		}
	}

	providerID := servingProvider
	if providerID == "" {
		providerID = lastProvider
	}
	if providerID == "" {
		return routingTraceBody{}, false
	}
	model := servingModel
	if model == "" {
		model = lastModel
	}

	// A fallback only happened when a failed attempt preceded the serving
	// attempt AND the serving target moved. A same-provider, same-model retry
	// is an internal availability retry, not a routing switch (the same rule
	// X-OpenRouter-Fallback uses, so the two channels cannot disagree).
	fallbackUsed := servingProvider != "" && firstFailedReason != ""
	if fallbackUsed && firstFailedProvider == servingProvider && firstFailedModel == servingModel {
		fallbackUsed = false
	}
	// All candidates failing is not a fallback on a success body; the error
	// path keeps its existing contract and never reaches this renderer.

	switchReason := RoutingTraceSwitchReasonNone
	if fallbackUsed {
		switchReason = firstFailedReason
	}

	trace := routingTraceBody{
		SchemaVersion: RoutingTraceSchemaVersion,
		RequestID:     logging.GetRequestID(ctx),
		ProviderID:    providerID,
		Model:         model,
		FallbackUsed:  fallbackUsed,
		SwitchReason:  switchReason,
	}
	if !full {
		return trace, true
	}
	decisionPath := make([]string, 0, 2)
	if routerRouted {
		decisionPath = append(decisionPath, "model_router")
	} else {
		decisionPath = append(decisionPath, "primary")
	}
	if fallbackUsed {
		decisionPath = append(decisionPath, "fallback")
	}
	trace.DecisionPath = decisionPath
	trace.Candidates = candidates
	trace.Attempts = failedAttempts
	if !startedAt.IsZero() {
		if latency := time.Since(startedAt); latency >= 0 {
			trace.LatencyMS = latency.Milliseconds()
		}
	}
	return trace, true
}

// routingTraceSwitchReason maps one failed attempt to the bounded
// machine-readable switch_reason contract. The vocabulary is deliberately
// small and stable: "timeout" for a deadline/504 class failure, "http_<code>"
// when an upstream status is known, and the sanitized event code (or
// "provider_error") otherwise.
func routingTraceSwitchReason(event coreexecutor.RouteEvent) string {
	code := strings.ToLower(strings.TrimSpace(event.Code))
	description := strings.ToLower(event.Error)
	if event.Status == 504 || strings.Contains(code, "timeout") || strings.Contains(code, "deadline") ||
		strings.Contains(description, "timeout") || strings.Contains(description, "deadline exceeded") {
		return "timeout"
	}
	status := event.Status
	if status == 0 {
		status = routingTraceTrailingStatus(code)
	}
	if status >= 400 && status <= 599 {
		return "http_" + strconv.Itoa(status)
	}
	if code != "" {
		return routingTraceSafeCode(code)
	}
	return "provider_error"
}

// routingTraceTrailingStatus extracts an HTTP status embedded in an engine
// reason code such as "provider_openai_503" when the event carried no status.
func routingTraceTrailingStatus(code string) int {
	end := len(code)
	start := end
	for start > 0 && code[start-1] >= '0' && code[start-1] <= '9' {
		start--
	}
	if start == end || end-start != 3 {
		return 0
	}
	status, errAtoi := strconv.Atoi(code[start:end])
	if errAtoi != nil {
		return 0
	}
	return status
}

// routingTraceSafeCode bounds and sanitizes a fallback reason code so it can
// never carry control characters or grow without limit.
func routingTraceSafeCode(code string) string {
	sanitized := strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e || unicode.IsSpace(r) {
			return '_'
		}
		return r
	}, strings.TrimSpace(code))
	if len(sanitized) > fallbackReasonMaxLen {
		sanitized = sanitized[:fallbackReasonMaxLen]
	}
	if sanitized == "" {
		return "provider_error"
	}
	return sanitized
}
