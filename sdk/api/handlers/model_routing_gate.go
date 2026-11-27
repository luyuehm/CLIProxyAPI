package handlers

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"golang.org/x/net/context"
)

// ModelRoutingRequest is one request-level model-routing decision input
// (RIC-3351).
type ModelRoutingRequest struct {
	// Body is the client request body the decision engine scores.
	Body []byte
	// RequestedModel is the model the client asked for (the audit echo and
	// the cost-tie breaker).
	RequestedModel string
	// Allowed reports whether the gateway can actually serve a candidate
	// model for THIS request: a provider must resolve for it, and its
	// modality must match the endpoint. The gate may only choose from
	// priced models this predicate accepts.
	//
	// The handler always supplies it. Without this constraint the engine
	// would pick the cheapest row of the whole KEEPER price table and could
	// route a chat request to an image-only model (503 from the image-only
	// guard) or to a model with no credential (400 model_not_found) —
	// turning a working request into a failure, which is exactly what the
	// routing degradation contract forbids.
	Allowed func(model string) bool
	// AllowedGeneration versions the Allowed answer. The gate caches the
	// filtered candidate set and reuses it while both this value and the price
	// snapshot are unchanged, so the per-model registry lookup runs once per
	// (credential set, price table) pair instead of once per request.
	// Zero disables caching.
	AllowedGeneration uint64
}

// ModelRoutingOutcome is one request-level model-routing decision as the
// gateway consumes it.
//
//	Model   — the model to execute instead of the requested one. Empty keeps
//	          the requested model, which is also the "routing declined" signal.
//	Headers — explainability headers to set on the response (x-or-model /
//	          x-or-reason), already header-safe.
//	Error   — a terminal routing rejection (402 budget_exceeded, 503
//	          model_unavailable, 400 capability constraint). When non-nil the
//	          request is answered with it and never reaches an upstream
//	          executor.
type ModelRoutingOutcome struct {
	Model   string
	Headers http.Header
	Error   *interfaces.ErrorMessage
}

// ModelRoutingGate applies the request-level model routing strategy engine
// (internal/modelrouting) to one request. It is installed through the
// pre-reserved api.WithRouterConfigurator hook, the same extension point the
// plugin model router host, the arbitrage model router host and the KEEPER
// route-boundary gate use, so no upstream handler or route file is modified.
//
// A nil gate (the default build) leaves model resolution exactly as before.
type ModelRoutingGate interface {
	// RouteRequest resolves the caller's in-force strategy, applies it to the
	// request, and returns the model to execute plus the explainability
	// headers and any terminal rejection.
	RouteRequest(ctx context.Context, req ModelRoutingRequest) ModelRoutingOutcome
}

// SetModelRoutingGate configures the optional request-level model routing
// gate (RIC-3351). A nil gate restores the default model resolution.
func (h *BaseAPIHandler) SetModelRoutingGate(gate ModelRoutingGate) {
	if h == nil {
		return
	}
	if isNilInterface(gate) {
		h.ModelRoutingGate = nil
		return
	}
	h.ModelRoutingGate = gate
}

// shouldApplyModelRouting reports whether the request-level routing gate may
// run for this execution. The gate only ever refines the plain
// model-to-provider path; it yields to every decision already made for the
// request:
//
//   - a plugin/arbitrage route decision (that router chose the executor,
//     provider or model),
//   - a nested plugin execution excluded from routing,
//   - a provider-pinned execution (execOptions.ForcedProvider): the caller
//     demanded one provider, so substituting another provider's model would
//     send a foreign model name upstream and turn a working request into a
//     4xx.
func shouldApplyModelRouting(decision modelRouteDecision, execOptions modelExecutionOptions) bool {
	if strings.TrimSpace(execOptions.SkipRouterPluginID) != "" {
		return false
	}
	if strings.TrimSpace(execOptions.ForcedProvider) != "" {
		return false
	}
	return strings.TrimSpace(decision.ExecutorPluginID) == "" &&
		strings.TrimSpace(decision.Provider) == "" &&
		strings.TrimSpace(decision.Model) == ""
}

// mediaHandlerTypes are the handler types whose requests are not text
// completions. A text price table has nothing useful to say about them, and
// substituting a chat model would dispatch an image/video job to a text model.
//
// The list is keyed on handlerType rather than the allowImageModel flag because
// that flag is opt-in per handler: image handlers set it, the video handlers
// call the plain execution path, so a flag-only guard silently routed video
// requests. Asking "which endpoint is this" cannot be forgotten by a new media
// handler the same way.
var mediaHandlerTypes = map[string]struct{}{
	"openai-image": {},
	"openai-video": {},
}

// mediaRequestPathPrefixes are the endpoint prefixes whose requests are media
// jobs.
//
// The request path is checked in addition to the handler type because one media
// endpoint can be served through more than one handler type:
// /v1/images/generations and /v1/images/edits run through "openai-image" on the
// xAI / OpenAI-compat branches but through "openai-response" on the default
// Responses-backed branch (openai_images_handlers.go collectImagesFromResponses
// / streamImagesFromResponses). "openai-response" is also the genuine text
// Responses API, which must stay routable, so the handler type cannot identify
// that branch — the URL can.
var mediaRequestPathPrefixes = []string{"/v1/images", "/v1/videos"}

// isMediaHandlerType reports whether handlerType names a media endpoint.
func isMediaHandlerType(handlerType string) bool {
	_, ok := mediaHandlerTypes[strings.ToLower(strings.TrimSpace(handlerType))]
	return ok
}

// isMediaRequestPath reports whether the request path is a media endpoint.
func isMediaRequestPath(ctx context.Context) bool {
	ginCtx, ok := ctx.Value("gin").(*gin.Context)
	if !ok || ginCtx == nil || ginCtx.Request == nil || ginCtx.Request.URL == nil {
		return false
	}
	path := strings.ToLower(ginCtx.Request.URL.Path)
	for _, prefix := range mediaRequestPathPrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// modelRoutingRequest builds the gate input for a request, or reports
// ok=false when routing must not touch it at all. The gate stays off for:
//
//   - media requests — by request path, by handler type, or when the caller
//     flagged the request as an image execution. All three are checked: no
//     single signal covers every media route (see the comments above);
//   - a requested model carrying a thinking-budget suffix ("model(8192)"):
//     routing to another model would silently drop the client's declared
//     budget, which is a behaviour change no cost saving justifies.
//
// In every other case the returned Allowed predicate restricts the candidate
// pool to models this gateway can actually serve.
func (h *BaseAPIHandler) modelRoutingRequest(handlerType string, body []byte, requestedModel string, allowImageModel bool) (ModelRoutingRequest, bool) {
	requested := strings.TrimSpace(requestedModel)
	if requested == "" || allowImageModel || isMediaHandlerType(handlerType) {
		return ModelRoutingRequest{}, false
	}
	if thinking.ParseSuffix(requested).HasSuffix {
		return ModelRoutingRequest{}, false
	}
	return ModelRoutingRequest{
		Body:              body,
		RequestedModel:    requested,
		Allowed:           h.modelRoutingAllowedFunc(),
		AllowedGeneration: registry.GetGlobalRegistry().GetGeneration(),
	}, true
}

// modelRoutingAllowedFunc builds the candidate predicate: a priced model is
// routable only when its modality matches a chat/text endpoint and a provider
// resolves for it.
//
// The provider test goes through registry.HasModelProvider rather than
// util.GetProviderName: the predicate runs once per priced model on every
// request, and GetProviderName builds and sorts a provider slice each time
// (~50µs per request at 200 priced models, measured). HasModelProvider answers
// the same question with a read lock and a map lookup.
func (h *BaseAPIHandler) modelRoutingAllowedFunc() func(string) bool {
	registry := registry.GetGlobalRegistry()
	return func(model string) bool {
		base := strings.TrimSpace(thinking.ParseSuffix(model).ModelName)
		if base == "" {
			base = strings.TrimSpace(model)
		}
		if base == "" {
			return false
		}
		// Text endpoints never accept an image-only model (the exec path
		// answers 503 for one), nor a video-only model (it would be dispatched
		// as if it were a chat model).
		if isOpenAIImageOnlyModel(base) || isVideoOnlyModel(base) {
			return false
		}
		// The home channel serves every model through one provider.
		if h != nil && h.AuthManager != nil && h.AuthManager.HomeEnabled() {
			return true
		}
		return registry.HasModelProvider(base)
	}
}

// applyModelRouting consults the installed gate and returns the model to
// execute. It is a no-op (returns modelName unchanged) when no gate is
// installed or the request is out of the gate's remit, so the default build's
// request path is byte-for-byte unchanged.
//
// The gate's explainability headers are written onto the response writer
// immediately: every execution path (non-streaming, streaming, counts) shares
// this call, and the response has not been written yet at this point.
func (h *BaseAPIHandler) applyModelRouting(ctx context.Context, handlerType string, body []byte, modelName string, allowImageModel bool) (string, *interfaces.ErrorMessage) {
	if h == nil || h.ModelRoutingGate == nil {
		return modelName, nil
	}
	// The path check comes first: it is the only signal that covers
	// /v1/images/* served through the Responses-backed branch, where the
	// handler type is the same as the text Responses API.
	if isMediaRequestPath(ctx) {
		return modelName, nil
	}
	req, ok := h.modelRoutingRequest(handlerType, body, modelName, allowImageModel)
	if !ok {
		return modelName, nil
	}
	outcome := h.ModelRoutingGate.RouteRequest(ctx, req)
	if len(outcome.Headers) > 0 {
		if ginCtx, okGin := ctx.Value("gin").(*gin.Context); okGin && ginCtx != nil {
			headers := ginCtx.Writer.Header()
			for key, values := range outcome.Headers {
				for _, value := range values {
					headers.Set(key, value)
				}
			}
		}
	}
	if outcome.Error != nil {
		return "", outcome.Error
	}
	if model := strings.TrimSpace(outcome.Model); model != "" {
		return model, nil
	}
	return modelName, nil
}
