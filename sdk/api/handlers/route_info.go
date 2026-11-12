package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"

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
	// routeInfoResponseField is the non-streaming response body field.
	routeInfoResponseField = "route_info"
)

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
	events       []coreexecutor.RouteEvent
}

// NewRouteInfoCollector builds a collector for one request. candidates is the
// ordered provider list resolved for the request; routerRouted reports whether
// a model router pinned the final provider.
func NewRouteInfoCollector(candidates []string, routerRouted bool) *RouteInfoCollector {
	collector := &RouteInfoCollector{candidates: candidates, routerRouted: routerRouted}
	return collector
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

// routeInfoHeaders ensures a header map suitable for the X-Route-Detail
// response header. Streaming responses normally carry no downstream header map
// unless passthrough is enabled, so a collector needs one allocated.
func routeInfoHeaders(headers http.Header, collector *RouteInfoCollector) http.Header {
	if collector == nil {
		return headers
	}
	if headers == nil {
		return make(http.Header)
	}
	return headers
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
