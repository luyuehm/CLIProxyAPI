package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"

	"github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"golang.org/x/net/context"
)

const (
	// RouteInfoDebugQueryParam is the opt-in query parameter (?debug=true)
	// that makes responses carry routing-decision metadata.
	RouteInfoDebugQueryParam = "debug"
	// RouteDetailResponseHeader carries the route_info JSON. It is the only
	// channel available for streaming responses, where the body cannot be
	// mutated after the first chunk is committed.
	RouteDetailResponseHeader = "X-Route-Detail"
	// OpenRouterFallbackHeader is the always-on response header that reports
	// a fallback when one actually occurred during a request. It is absent
	// when no fallback happened: a direct success, a request with no routing
	// events, or a terminal failure where every candidate failed (the error
	// body carries the final error).
	OpenRouterFallbackHeader = "X-OpenRouter-Fallback"
	// routeInfoResponseField is the non-streaming response body field.
	routeInfoResponseField = "route_info"
)

// fallbackHeaderMaxLen bounds the compact X-OpenRouter-Fallback value so a
// verbose sanitized reason cannot exceed HTTP header size limits. The
// fallback-reason suffix is capped tighter (fallbackReasonMaxLen).
const (
	fallbackHeaderMaxLen = 512
	fallbackReasonMaxLen = 200
)

// routeInfoLog is the package-level structured logger for the routing
// observability surface.
var routeInfoLog = logrus.WithField("component", "handlers.route_info")

// routeInfoMaxFallbackEvents bounds the per-request fallback event list so a
// pathological retry loop cannot bloat the response.
const routeInfoMaxFallbackEvents = 16

// RouteInfoCollector records the credential-level routing attempts of one
// request and renders them as the route_info payload. It implements
// coreexecutor.RouteInfoReporter so the auth conductor can publish picks and
// failures while the request executes. When the debug parameter is off no
// collector is created and the conductor's reporting path is a no-op.
type RouteInfoCollector struct {
	mu           sync.Mutex
	candidates   []string
	routerRouted bool
	model        string
	events       []coreexecutor.RouteEvent
}

// NewRouteInfoCollector builds a collector for one request. candidates is the
// ordered provider list resolved for the request; routerRouted reports whether
// a model router pinned the final provider.
func NewRouteInfoCollector(candidates []string, routerRouted bool) *RouteInfoCollector {
	return NewRouteInfoCollectorWithModel(candidates, routerRouted, "")
}

// NewRouteInfoCollectorWithModel builds a collector for one request and keeps
// the normalized model as a fallback when no route event carries a model.
func NewRouteInfoCollectorWithModel(candidates []string, routerRouted bool, model string) *RouteInfoCollector {
	return &RouteInfoCollector{candidates: candidates, routerRouted: routerRouted, model: model}
}

// ReportEvent records one routing attempt. Safe for concurrent use.
func (c *RouteInfoCollector) ReportEvent(event coreexecutor.RouteEvent) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.events) < routeInfoMaxFallbackEvents*2 {
		c.events = append(c.events, event)
	}
}

// routeInfoEventBody is one failed credential attempt in the client payload.
type routeInfoEventBody struct {
	FailedProvider string `json:"failed_provider"`
	AuthIndex      string `json:"auth_index,omitempty"`
	Model          string `json:"model,omitempty"`
	Status         int    `json:"status,omitempty"`
	Code           string `json:"code,omitempty"`
	Error          string `json:"error,omitempty"`
	Timestamp      string `json:"timestamp,omitempty"`
}

// routeInfoBody is the route_info object returned to the client.
type routeInfoBody struct {
	RequestID       string               `json:"request_id,omitempty"`
	FinalProvider   string               `json:"final_provider,omitempty"`
	SelectionReason string               `json:"selection_reason"`
	Candidates      []string             `json:"candidates"`
	FallbackEvents  []routeInfoEventBody `json:"fallback_events"`
}

// buildBody renders the collected routing outcome. A nil collector yields an
// empty, valid payload so callers can invoke it unconditionally.
func (c *RouteInfoCollector) buildBody(ctx context.Context) routeInfoBody {
	if c == nil {
		return routeInfoBody{Candidates: []string{}, FallbackEvents: []routeInfoEventBody{}, SelectionReason: "no routing information collected"}
	}
	c.mu.Lock()
	events := append([]coreexecutor.RouteEvent(nil), c.events...)
	candidates := append([]string(nil), c.candidates...)
	c.mu.Unlock()

	finalProvider := ""
	fallbackEvents := make([]routeInfoEventBody, 0)
	for _, event := range events {
		switch event.Outcome {
		case coreexecutor.RouteOutcomeSuccess:
			finalProvider = event.Provider
		default:
			if len(fallbackEvents) >= routeInfoMaxFallbackEvents {
				continue
			}
			fallbackEvents = append(fallbackEvents, routeInfoEventBody{
				FailedProvider: event.Provider,
				AuthIndex:      event.AuthIndex,
				Model:          event.Model,
				Status:         event.Status,
				Code:           event.Code,
				Error:          event.Error,
				Timestamp:      event.Timestamp.UTC().Format("2006-01-02T15:04:05.000Z"),
			})
		}
	}
	sort.SliceStable(fallbackEvents, func(i, j int) bool { return fallbackEvents[i].Timestamp < fallbackEvents[j].Timestamp })
	if len(candidates) == 0 {
		candidates = []string{}
	}

	body := routeInfoBody{
		RequestID:      logging.GetRequestID(ctx),
		FinalProvider:  finalProvider,
		Candidates:     candidates,
		FallbackEvents: fallbackEvents,
	}
	body.SelectionReason = c.selectionReason(finalProvider, len(fallbackEvents))
	return body
}

// selectionReason explains how the final provider was chosen. It is always
// non-empty so the payload stays auditable even when nothing failed.
func (c *RouteInfoCollector) selectionReason(finalProvider string, failedAttempts int) string {
	switch {
	case finalProvider == "":
		return "no provider execution completed"
	case c.routerRouted && len(c.candidates) == 1 && failedAttempts == 0:
		return fmt.Sprintf("model router routed to provider %q", finalProvider)
	case failedAttempts > 0:
		return fmt.Sprintf("fallback: provider %q served the request after %d failed attempt(s)", finalProvider, failedAttempts)
	case len(c.candidates) > 1:
		return fmt.Sprintf("primary: provider %q selected from %d candidate provider(s)", finalProvider, len(c.candidates))
	default:
		return fmt.Sprintf("primary: single candidate provider %q selected", finalProvider)
	}
}

// OpenRouterFallback renders the always-on fallback marker for one request.
// It returns "" when no fallback actually occurred, so the header is absent:
//
//   - a direct success with no failed attempts (no fallback configured);
//   - a terminal failure where every candidate failed (the final error is in
//     the response body, not the header);
//   - a nil collector or no routing events (nothing to report).
//
// When a failed attempt was followed by a serving success, the value is the
// compact "failedModel->servingModel" chain plus the sanitized failure reason
// of the first failed attempt, header-safe and bounded:
//
//	X-OpenRouter-Fallback: modelA->modelB (reason: provider_A_503)
func (c *RouteInfoCollector) OpenRouterFallback() string {
	if c == nil {
		return ""
	}
	attempts := c.attemptsSnapshot()
	if len(attempts) == 0 {
		return ""
	}
	failedModel, failedProvider, failedReason, servingModel, servingProvider := "", "", "", "", ""
	for _, event := range attempts {
		if event.Outcome == coreexecutor.RouteOutcomeSuccess {
			servingModel = event.Model
			servingProvider = event.Provider
			break
		}
		if failedModel == "" && event.Model != "" {
			failedModel = event.Model
		}
		if failedProvider == "" && event.Provider != "" {
			failedProvider = event.Provider
		}
		if failedReason == "" {
			failedReason = releasableRouteReason(event)
		}
	}
	// A fallback marker is only emitted when a failed attempt preceded a
	// serving success AND the serving decision moved to a different model or
	// provider (a real routing fallback). A direct success (scenario 3) and
	// an all-failed terminal response (scenario 2) leave it absent; so does
	// a same-model-and-provider credential retry, where the "service path"
	// the client sees did not change.
	if servingModel == "" || failedReason == "" {
		return ""
	}
	if failedModel == servingModel && failedProvider == servingProvider {
		return ""
	}
	// The pair names what the client can see: models when they differ,
	// otherwise — same model, different provider — the provider keys the
	// request moved between.
	from, to := failedModel, servingModel
	if from == to || from == "" {
		from = failedProvider
		if from == "" {
			from = to
		}
		to = servingProvider
		if to == "" {
			to = from
		}
	}
	// The move must be distinguishable; identical pair => no marker.
	if from == to {
		return ""
	}
	value := fallbackHeaderSafe(from) + "->" + fallbackHeaderSafe(to) +
		" (reason: " + fallbackHeaderSafe(failedReason) + ")"
	value = fallbackHeaderSafe(value)
	if len(value) > fallbackHeaderMaxLen {
		value = value[:fallbackHeaderMaxLen]
	}
	return value
}

// LogFallbackEvents emits the fallback decision trail as one structured Info
// log line, so request logs carry which providers/models failed and whether a
// later candidate served the request or the whole chain failed. It is a
// no-op when no attempt failed (no fallback trail to record), keeping the
// default request log free of routing noise.
func (c *RouteInfoCollector) LogFallbackEvents() {
	if c == nil {
		return
	}
	attempts := c.attemptsSnapshot()
	if len(attempts) == 0 {
		return
	}
	failed := make([]map[string]any, 0, len(attempts))
	served := ""
	for _, event := range attempts {
		if event.Outcome == coreexecutor.RouteOutcomeSuccess {
			served = event.Model
			if served == "" {
				served = event.Provider
			}
			break
		}
		failed = append(failed, map[string]any{
			"provider": event.Provider,
			"model":    event.Model,
			"status":   event.Status,
			"code":     event.Code,
			"reason":   releasableRouteReason(event),
		})
	}
	if len(failed) == 0 {
		return
	}
	fields := logrus.Fields{"fallback_chain": failed}
	if served != "" {
		fields["served_model"] = served
		routeInfoLog.WithFields(fields).Info("route: fallback event served")
	} else {
		routeInfoLog.WithFields(fields).Warn("route: fallback chain exhausted")
	}
}

// attemptsSnapshot returns a copy of the collected events under lock.
func (c *RouteInfoCollector) attemptsSnapshot() []coreexecutor.RouteEvent {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]coreexecutor.RouteEvent(nil), c.events...)
}

// releasableRouteReason picks the client-safe failure text for one failed
// route event. The event's Code (a bounded machine code like "upstream_503")
// is preferred; the sanitized human Error is used only when no code exists.
// Both are already client-safe at the reporting site, but the value is capped
// again here so a lengthy message cannot bloat the header.
func releasableRouteReason(event coreexecutor.RouteEvent) string {
	if code := strings.TrimSpace(event.Code); code != "" {
		return code
	}
	message := strings.TrimSpace(event.Error)
	if len(message) > fallbackReasonMaxLen {
		message = message[:fallbackReasonMaxLen]
	}
	return message
}

// fallbackHeaderSafe strips non-printable ASCII so a client-supplied model
// name or error text can never inject a newline or escape sequence into a
// response header value.
func fallbackHeaderSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 0x20 && r <= 0x7e {
			return r
		}
		return -1
	}, s)
}

// JSON marshals the route_info body, or returns "" on failure. A nil
// collector returns "" so callers can skip the header without a guard.
func (c *RouteInfoCollector) JSON(ctx context.Context) string {
	if c == nil {
		return ""
	}
	data, errMarshal := json.Marshal(c.buildBody(ctx))
	if errMarshal != nil {
		return ""
	}
	return string(data)
}

// InjectIntoJSON adds the route_info field to a non-streaming JSON response
// body. It returns the body unchanged when the payload is not a JSON object:
// sjson would otherwise replace the whole document.
func (c *RouteInfoCollector) InjectIntoJSON(ctx context.Context, body []byte) []byte {
	if c == nil || len(body) == 0 || !gjson.ValidBytes(body) {
		return body
	}
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return body
	}
	injected, errSet := sjson.SetBytes(body, routeInfoResponseField, c.buildBody(ctx))
	if errSet != nil || len(injected) == 0 {
		return body
	}
	return injected
}

// routeInfoDebugEnabled reports whether the request opted into routing
// metadata via the debug query parameter.
func routeInfoDebugEnabled(ctx context.Context) bool {
	values := queryFromContext(ctx)
	return debugQueryParamEnabled(values)
}

// debugQueryParamEnabled parses the debug parameter; only explicit truthy
// values enable reporting, so a stray ?debug= is a no-op.
func debugQueryParamEnabled(values url.Values) bool {
	if values == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(values.Get(RouteInfoDebugQueryParam))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// newRouteInfoCollector returns a collector for the request when the debug
// parameter enabled it, otherwise nil (zero reporting cost).
func (h *BaseAPIHandler) newRouteInfoCollector(ctx context.Context, candidates []string, routerRouted bool) *RouteInfoCollector {
	if !routeInfoDebugEnabled(ctx) {
		return nil
	}
	return NewRouteInfoCollector(candidates, routerRouted)
}

// newAlwaysRouteInfoCollector returns a collector for every non-streaming
// request so the gateway can always emit X-OpenRouter-Fallback and log the
// fallback trail. Streaming responses keep the debug-opt-in collector because
// their body is committed before the full routing outcome is known.
func (h *BaseAPIHandler) newAlwaysRouteInfoCollector(candidates []string, routerRouted bool, model string) *RouteInfoCollector {
	return NewRouteInfoCollectorWithModel(candidates, routerRouted, model)
}

// attachOpenRouterFallback sets X-OpenRouter-Fallback on a non-streaming
// response header map. It allocates the map when needed and is a no-op when
// the collector reports no fallback (direct success or terminal failure).
func attachOpenRouterFallback(headers http.Header, collector *RouteInfoCollector) http.Header {
	if collector == nil {
		return headers
	}
	value := collector.OpenRouterFallback()
	if value == "" {
		return headers
	}
	if headers == nil {
		headers = make(http.Header)
	}
	headers.Set(OpenRouterFallbackHeader, value)
	return headers
}

// logFallbackIfAny emits the structured fallback event log for the request.
// It is a no-op when the collector is nil or the request did not fall back.
func (c *RouteInfoCollector) logFallbackIfAny() {
	c.LogFallbackEvents()
}

// attachRouteInfoToHeaders sets the X-Route-Detail response header on headers
// for streaming responses. It is a no-op for a nil collector.
func attachRouteInfoToHeaders(headers http.Header, collector *RouteInfoCollector, ctx context.Context) {
	if collector == nil {
		return
	}
	if value := collector.JSON(ctx); value != "" {
		headers.Set(RouteDetailResponseHeader, value)
	}
}
