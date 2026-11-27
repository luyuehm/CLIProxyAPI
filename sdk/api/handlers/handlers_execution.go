package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/clienterror"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/fallback"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"golang.org/x/net/context"
)

// PluginExecutorHost executes a routed request with a specific plugin executor.
type PluginExecutorHost interface {
	ExecutePluginExecutor(context.Context, string, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error)
	ExecutePluginExecutorStream(context.Context, string, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error)
	CountPluginExecutor(context.Context, string, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error)
}

type pluginExecutorFormatResolver interface {
	PluginExecutorRequestToFormat(string, coreexecutor.Request, coreexecutor.Options) sdktranslator.Format
}

// ExecuteWithAuthManager executes a non-streaming request via the core auth manager.
// This path is the only supported execution route.
func (h *BaseAPIHandler) ExecuteWithAuthManager(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string) ([]byte, http.Header, *interfaces.ErrorMessage) {
	return h.executeWithAuthManager(ctx, handlerType, modelName, rawJSON, alt, false)
}

// ExecuteImageWithAuthManager executes an OpenAI-compatible image endpoint request.
func (h *BaseAPIHandler) ExecuteImageWithAuthManager(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string) ([]byte, http.Header, *interfaces.ErrorMessage) {
	return h.executeWithAuthManager(ctx, handlerType, modelName, rawJSON, alt, true)
}

func (h *BaseAPIHandler) executeWithAuthManager(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string, allowImageModel bool) ([]byte, http.Header, *interfaces.ErrorMessage) {
	return h.executeWithAuthManagerFormats(ctx, handlerType, handlerType, modelName, rawJSON, alt, allowImageModel, modelExecutionOptions{})
}

func (h *BaseAPIHandler) executeWithAuthManagerFormats(ctx context.Context, entryProtocol, exitProtocol, modelName string, rawJSON []byte, alt string, allowImageModel bool, execOptions modelExecutionOptions) ([]byte, http.Header, *interfaces.ErrorMessage) {
	originalRequestedModel := modelName
	routeDecision := h.applyModelRouter(ctx, entryProtocol, modelName, rawJSON, false, execOptions)
	// RIC-3351: apply the request-level model routing strategy on the plain
	// model-to-provider path. A nil gate (the default build) is a no-op.
	if shouldApplyModelRouting(routeDecision, execOptions) {
		routedModel, routingErr := h.applyModelRouting(ctx, entryProtocol, rawJSON, modelName, allowImageModel)
		if routingErr != nil {
			return nil, nil, routingErr
		}
		modelName = routedModel
	}
	responseProtocol := modelExecutionResponseProtocol(entryProtocol, exitProtocol)
	if errMsg := validateNativeInteractionsExecution(entryProtocol, execOptions, routeDecision); errMsg != nil {
		return nil, nil, errMsg
	}
	if routeDecision.ExecutorPluginID != "" {
		return h.executeWithPluginExecutor(ctx, entryProtocol, responseProtocol, modelName, originalRequestedModel, rawJSON, alt, routeDecision.ExecutorPluginID, execOptions)
	}
	providers, normalizedModel, errMsg := h.providersForExecution(modelName, originalRequestedModel, allowImageModel, routeDecision, execOptions)
	if errMsg != nil {
		return nil, nil, errMsg
	}
	providers = adjustExecutionProvidersForEntryProtocol(entryProtocol, providers)
	// RIC-2532-A: narrow the pool to the caller's KEEPER route boundary
	// before execution; a nil gate leaves it untouched.
	providers, errMsg = h.applyRouteBoundary(ctx, normalizedModel, providers, routeDecision, execOptions)
	if errMsg != nil {
		return nil, nil, errMsg
	}
	// RIC-3352: apply the request-level provider object (OpenRouter provider
	// prefs) on the boundary-narrowed pool. A nil record (no provider object)
	// leaves the pool and the configured fallback chain untouched.
	prefsRec := h.applyProviderPreferences(ctx, providers, rawJSON, normalizedModel)
	if prefsRec != nil {
		providers = prefsRec.pool
		if prefsRec.violation != nil {
			// The KEEPER allow-list forbids the request's provider selection:
			// surface the boundary verdict before any upstream request is made.
			return nil, nil, prefsRec.violation
		}
	}
	reqMeta := requestExecutionMetadata(ctx)
	reqMeta[coreexecutor.RequestedModelMetadataKey] = originalRequestedModel
	addAuthSelectionModelMetadata(reqMeta, execOptions.AuthSelectionModel)
	addModelExecutionSourceMetadata(reqMeta, execOptions.InternalSource)
	setReasoningEffortMetadata(reqMeta, entryProtocol, normalizedModel, rawJSON)
	setServiceTierMetadata(reqMeta, rawJSON)
	setGenerateMetadata(reqMeta, rawJSON)
	payload := rawJSON
	if len(payload) == 0 {
		payload = nil
	}
	req := coreexecutor.Request{
		Model:   normalizedModel,
		Payload: payload,
	}
	afterAuthCapture := &requestAfterAuthCapture{}
	lifecycle := h.newRequestLifecycleTracker(ctx, entryProtocol, normalizedModel, originalRequestedModel, false, reqMeta, execOptions.SkipInterceptorPluginID)
	opts := coreexecutor.Options{
		Stream:                      false,
		Alt:                         alt,
		OriginalRequest:             rawJSON,
		SourceFormat:                sdktranslator.FromString(entryProtocol),
		ResponseFormat:              sdktranslator.FromString(responseProtocol),
		Headers:                     modelExecutionHeaders(ctx, execOptions.Headers),
		Query:                       modelExecutionQuery(ctx, execOptions.Query),
		RequestAfterAuthInterceptor: h.requestAfterAuthInterceptor(afterAuthCapture, lifecycle.requestID(), execOptions.SkipInterceptorPluginID),
		WebSocketResponseObserver:   h.webSocketResponseObserver(lifecycle.requestID(), execOptions.SkipInterceptorPluginID),
	}
	opts.Metadata = reqMeta
	ctx = enrichContextWithSessionHierarchy(ctx, opts.Headers, req.Payload, opts.Metadata)
	routeInfoCollector := h.newAlwaysRouteInfoCollector(providers, routeDecision.Provider != "", normalizedModel)
	if routeInfoCollector != nil {
		opts.Metadata[coreexecutor.RouteInfoReporterMetadataKey] = routeInfoCollector
		// RIC-3352: thread the request-level provider-object outcome into the
		// trace so routing_trace.provider_prefs_* renders it (no-prefs
		// requests leave the lists nil and the fields stay omitted).
		if prefsRec != nil {
			routeInfoCollector.SetProviderPrefs(prefsRec.applied, prefsRec.ignored)
		}
	}
	var interceptErr *interfaces.ErrorMessage
	req, opts, interceptErr = h.applyRequestInterceptorsBeforeAuth(ctx, entryProtocol, originalRequestedModel, lifecycle.requestID(), req, opts, execOptions.SkipInterceptorPluginID)
	if interceptErr != nil {
		setOpenRouterRouteError(interceptErr, routeInfoCollector, ctx)
		lifecycle.completeError(ctx, interceptErr)
		return nil, nil, interceptErr
	}
	ctx = enrichContextWithSessionHierarchy(ctx, opts.Headers, req.Payload, opts.Metadata)
	engineOpts := fallback.RequestOptions{}
	if prefsRec != nil {
		engineOpts = prefsRec.engineOpts
	}
	resp, fallbackState, err := h.executeWithFallback(ctx, providers, req, opts, engineOpts)
	if err != nil {
		err = enrichAuthSelectionError(err, providers, normalizedModel)
		// RIC-2532-A: when the request ran inside a KEEPER route boundary
		// and the pool could not be served, surface the contract's 503
		// route_boundary_violation instead of the raw credential error.
		if boundaryErr := h.translateRouteBoundaryExecutionError(ctx, err, normalizedModel); boundaryErr != nil {
			setOpenRouterRouteError(boundaryErr, routeInfoCollector, ctx)
			routeInfoCollector.logFallbackIfAny()
			lifecycle.completeError(ctx, boundaryErr)
			return nil, nil, boundaryErr
		}
		// RIC-3143: a provider-level fallback that exhausted its chain is a
		// 503 carrying attempted_models, not a raw credential error. A
		// request that failed without falling back keeps its raw error.
		if fallbackErr := fallbackState.buildErrorMessage(); fallbackErr != nil {
			setOpenRouterRouteError(fallbackErr, routeInfoCollector, ctx)
			routeInfoCollector.logFallbackIfAny()
			lifecycle.completeError(ctx, fallbackErr)
			return nil, nil, fallbackErr
		}
		errMsg := executionErrorMessage(err)
		setOpenRouterRouteError(errMsg, routeInfoCollector, ctx)
		routeInfoCollector.logFallbackIfAny()
		lifecycle.completeError(ctx, errMsg)
		return nil, nil, errMsg
	}
	executedReq, executedOpts := afterAuthCapture.apply(req, opts)
	ctx = enrichContextWithSessionHierarchy(ctx, executedOpts.Headers, executedReq.Payload, executedOpts.Metadata)
	rawResponseHeaders := cloneHeader(resp.Headers)
	responseHeaders := downstreamHeadersFromExecutor(rawResponseHeaders, PassthroughHeadersEnabled(h.Cfg))
	body, responseHeaders := h.applyResponseInterceptors(ctx, lifecycle.requestID(), responseProtocol, normalizedModel, originalRequestedModel, executedOpts, rawResponseHeaders, responseHeaders, executedOpts.OriginalRequest, executedReq.Payload, resp.Payload, http.StatusOK, execOptions.SkipInterceptorPluginID)
	responseHeaders = attachOpenRouterRouteToHeaders(responseHeaders, routeInfoCollector, ctx)
	responseHeaders = attachOpenRouterFallback(responseHeaders, routeInfoCollector)
	responseHeaders = fallbackState.attachToHeaders(responseHeaders)
	routeInfoCollector.logFallbackIfAny()
	if routeInfoCollector != nil && routeInfoDebugEnabled(ctx) {
		body = routeInfoCollector.InjectIntoJSON(ctx, body)
	}
	if routingTraceRequested(entryProtocol, execOptions) {
		body = h.attachRoutingTrace(ctx, body, routeInfoCollector, fallbackState)
	}
	lifecycle.complete(pluginapi.RequestCompletionSucceeded, http.StatusOK, nil)
	return body, responseHeaders, nil
}

// ExecuteCountWithAuthManager executes a non-streaming request via the core auth manager.
// This path is the only supported execution route.
func (h *BaseAPIHandler) ExecuteCountWithAuthManager(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string) ([]byte, http.Header, *interfaces.ErrorMessage) {
	return h.executeCountWithAuthManager(ctx, handlerType, modelName, rawJSON, alt, modelExecutionOptions{})
}

func (h *BaseAPIHandler) executeCountWithAuthManager(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string, execOptions modelExecutionOptions) ([]byte, http.Header, *interfaces.ErrorMessage) {
	originalRequestedModel := modelName
	routeDecision := h.applyModelRouter(ctx, handlerType, modelName, rawJSON, false, execOptions)
	if routeDecision.ExecutorPluginID != "" {
		return h.countWithPluginExecutor(ctx, handlerType, modelName, originalRequestedModel, rawJSON, alt, routeDecision.ExecutorPluginID, execOptions)
	}
	// RIC-3351: apply the request-level model routing strategy on the plain
	// model-to-provider path. A nil gate (the default build) is a no-op.
	if shouldApplyModelRouting(routeDecision, execOptions) {
		routedModel, routingErr := h.applyModelRouting(ctx, handlerType, rawJSON, modelName, false)
		if routingErr != nil {
			return nil, nil, routingErr
		}
		modelName = routedModel
	}
	providers, normalizedModel, errMsg := h.providersForExecution(modelName, originalRequestedModel, false, routeDecision, execOptions)
	if errMsg != nil {
		return nil, nil, errMsg
	}
	providers = adjustExecutionProvidersForEntryProtocol(handlerType, providers)
	// RIC-2532-A: narrow the pool to the caller's KEEPER route boundary
	// before execution; a nil gate leaves it untouched.
	providers, errMsg = h.applyRouteBoundary(ctx, normalizedModel, providers, routeDecision, execOptions)
	if errMsg != nil {
		return nil, nil, errMsg
	}
	reqMeta := requestExecutionMetadata(ctx)
	reqMeta[coreexecutor.RequestedModelMetadataKey] = originalRequestedModel
	addAuthSelectionModelMetadata(reqMeta, execOptions.AuthSelectionModel)
	setReasoningEffortMetadata(reqMeta, handlerType, normalizedModel, rawJSON)
	setServiceTierMetadata(reqMeta, rawJSON)
	setGenerateMetadata(reqMeta, rawJSON)
	payload := rawJSON
	if len(payload) == 0 {
		payload = nil
	}
	req := coreexecutor.Request{
		Model:   normalizedModel,
		Payload: payload,
	}
	afterAuthCapture := &requestAfterAuthCapture{}
	lifecycle := h.newRequestLifecycleTracker(ctx, handlerType, normalizedModel, originalRequestedModel, false, reqMeta, execOptions.SkipInterceptorPluginID)
	opts := coreexecutor.Options{
		Stream:                      false,
		Alt:                         alt,
		OriginalRequest:             rawJSON,
		SourceFormat:                sdktranslator.FromString(handlerType),
		Headers:                     modelExecutionHeaders(ctx, execOptions.Headers),
		Query:                       modelExecutionQuery(ctx, execOptions.Query),
		RequestAfterAuthInterceptor: h.requestAfterAuthInterceptor(afterAuthCapture, lifecycle.requestID(), execOptions.SkipInterceptorPluginID),
		WebSocketResponseObserver:   h.webSocketResponseObserver(lifecycle.requestID(), execOptions.SkipInterceptorPluginID),
	}
	opts.Metadata = reqMeta
	ctx = enrichContextWithSessionHierarchy(ctx, opts.Headers, req.Payload, opts.Metadata)
	routeInfoCollector := h.newAlwaysRouteInfoCollector(providers, routeDecision.Provider != "", normalizedModel)
	if routeInfoCollector != nil {
		opts.Metadata[coreexecutor.RouteInfoReporterMetadataKey] = routeInfoCollector
	}
	var interceptErr *interfaces.ErrorMessage
	req, opts, interceptErr = h.applyRequestInterceptorsBeforeAuth(ctx, handlerType, originalRequestedModel, lifecycle.requestID(), req, opts, execOptions.SkipInterceptorPluginID)
	if interceptErr != nil {
		setOpenRouterRouteError(interceptErr, routeInfoCollector, ctx)
		lifecycle.completeError(ctx, interceptErr)
		return nil, nil, interceptErr
	}
	ctx = enrichContextWithSessionHierarchy(ctx, opts.Headers, req.Payload, opts.Metadata)
	resp, fallbackState, err := h.executeCountWithFallback(ctx, providers, req, opts)
	if err != nil {
		err = enrichAuthSelectionError(err, providers, normalizedModel)
		// RIC-2532-A: surface the contract's 503 route_boundary_violation
		// when a boundary-constrained pool could not be served.
		if boundaryErr := h.translateRouteBoundaryExecutionError(ctx, err, normalizedModel); boundaryErr != nil {
			setOpenRouterRouteError(boundaryErr, routeInfoCollector, ctx)
			routeInfoCollector.logFallbackIfAny()
			lifecycle.completeError(ctx, boundaryErr)
			return nil, nil, boundaryErr
		}
		if fallbackErr := fallbackState.buildErrorMessage(); fallbackErr != nil {
			setOpenRouterRouteError(fallbackErr, routeInfoCollector, ctx)
			routeInfoCollector.logFallbackIfAny()
			lifecycle.completeError(ctx, fallbackErr)
			return nil, nil, fallbackErr
		}
		errMsg := executionErrorMessage(err)
		setOpenRouterRouteError(errMsg, routeInfoCollector, ctx)
		routeInfoCollector.logFallbackIfAny()
		lifecycle.completeError(ctx, errMsg)
		return nil, nil, errMsg
	}
	executedReq, executedOpts := afterAuthCapture.apply(req, opts)
	ctx = enrichContextWithSessionHierarchy(ctx, executedOpts.Headers, executedReq.Payload, executedOpts.Metadata)
	rawResponseHeaders := cloneHeader(resp.Headers)
	responseHeaders := downstreamHeadersFromExecutor(rawResponseHeaders, PassthroughHeadersEnabled(h.Cfg))
	body, responseHeaders := h.applyResponseInterceptors(ctx, lifecycle.requestID(), handlerType, normalizedModel, originalRequestedModel, executedOpts, rawResponseHeaders, responseHeaders, executedOpts.OriginalRequest, executedReq.Payload, resp.Payload, http.StatusOK, execOptions.SkipInterceptorPluginID)
	responseHeaders = attachOpenRouterRouteToHeaders(responseHeaders, routeInfoCollector, ctx)
	responseHeaders = attachOpenRouterFallback(responseHeaders, routeInfoCollector)
	responseHeaders = fallbackState.attachToHeaders(responseHeaders)
	routeInfoCollector.logFallbackIfAny()
	// Count-token responses stay byte-for-byte unchanged: routing_trace is a
	// completion-response contract (RIC-3275) and a token counter is not a
	// routing audit surface.
	lifecycle.complete(pluginapi.RequestCompletionSucceeded, http.StatusOK, nil)
	return body, responseHeaders, nil
}

func (h *BaseAPIHandler) executeWithPluginExecutor(ctx context.Context, entryProtocol, responseProtocol, modelName, originalRequestedModel string, rawJSON []byte, alt, executorPluginID string, execOptions modelExecutionOptions) ([]byte, http.Header, *interfaces.ErrorMessage) {
	if h.AuthManager != nil && h.AuthManager.HomeEnabled() {
		return nil, nil, &interfaces.ErrorMessage{StatusCode: http.StatusServiceUnavailable, Error: fmt.Errorf("plugin executor routing is unavailable while Home is enabled")}
	}
	host := h.pluginExecutorHost()
	if host == nil {
		return nil, nil, &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: fmt.Errorf("plugin executor host is unavailable")}
	}
	routeInfoCollector := h.newAlwaysRouteInfoCollector(nil, false, modelName)
	execCtx, nestedTracker := withNestedExecutionTracker(coreusage.WithStream(ctx, false))
	req, opts := h.pluginExecutorRequest(execCtx, entryProtocol, responseProtocol, modelName, originalRequestedModel, rawJSON, alt, false, execOptions)
	lifecycle := h.newRequestLifecycleTracker(execCtx, entryProtocol, modelName, originalRequestedModel, false, opts.Metadata, execOptions.SkipInterceptorPluginID)
	if routeInfoCollector != nil {
		opts.Metadata[coreexecutor.RouteInfoReporterMetadataKey] = routeInfoCollector
	}
	var interceptErr *interfaces.ErrorMessage
	req, opts, interceptErr = h.applyRequestInterceptorsBeforeAuth(execCtx, entryProtocol, originalRequestedModel, lifecycle.requestID(), req, opts, execOptions.SkipInterceptorPluginID)
	if interceptErr != nil {
		setOpenRouterRouteError(interceptErr, routeInfoCollector, execCtx)
		lifecycle.completeError(execCtx, interceptErr)
		return nil, nil, interceptErr
	}
	req, opts, interceptErr = h.applyRequestInterceptorsAfterPluginExecutorRoute(execCtx, host, executorPluginID, entryProtocol, originalRequestedModel, lifecycle.requestID(), req, opts, execOptions.SkipInterceptorPluginID)
	if interceptErr != nil {
		setOpenRouterRouteError(interceptErr, routeInfoCollector, execCtx)
		lifecycle.completeError(execCtx, interceptErr)
		return nil, nil, interceptErr
	}
	execCtx = enrichContextWithSessionHierarchy(execCtx, opts.Headers, req.Payload, opts.Metadata)
	var reporter *helps.UsageReporter
	if !execOptions.InternalSource {
		reporter = helps.NewUsageReporter(execCtx, executorPluginID, modelName, nil)
		reporter.SetTranslatedReasoningEffort(req.Payload, entryProtocol)
	}
	resp, errExecute := host.ExecutePluginExecutor(execCtx, executorPluginID, req, opts)
	if errExecute != nil {
		if reporter != nil && !nestedTracker.hasNestedExecution() {
			reporter.PublishFailure(execCtx, errExecute)
		}
		routeInfoCollector.logFallbackIfAny()
		errMsg := executionErrorMessage(errExecute)
		setOpenRouterRouteError(errMsg, routeInfoCollector, execCtx)
		lifecycle.completeError(execCtx, errMsg)
		return nil, nil, errMsg
	}
	if reporter != nil && !nestedTracker.hasNestedExecution() {
		detail := parsePluginExecutorResponseUsage(responseProtocol, resp.Payload)
		reporter.Publish(execCtx, detail)
		reporter.EnsurePublished(execCtx)
	}
	rawResponseHeaders := cloneHeader(resp.Headers)
	responseHeaders := downstreamHeadersFromExecutor(rawResponseHeaders, PassthroughHeadersEnabled(h.Cfg))
	body, responseHeaders := h.applyResponseInterceptors(execCtx, lifecycle.requestID(), responseProtocol, modelName, originalRequestedModel, opts, rawResponseHeaders, responseHeaders, opts.OriginalRequest, req.Payload, resp.Payload, http.StatusOK, execOptions.SkipInterceptorPluginID)
	responseHeaders = attachOpenRouterRouteToHeaders(responseHeaders, routeInfoCollector, execCtx)
	responseHeaders = attachOpenRouterFallback(responseHeaders, routeInfoCollector)
	routeInfoCollector.logFallbackIfAny()
	if routingTraceRequested(entryProtocol, execOptions) {
		body = h.attachRoutingTrace(execCtx, body, routeInfoCollector, nil)
	}
	lifecycle.complete(pluginapi.RequestCompletionSucceeded, http.StatusOK, nil)
	return body, responseHeaders, nil
}

func (h *BaseAPIHandler) countWithPluginExecutor(ctx context.Context, handlerType, modelName, originalRequestedModel string, rawJSON []byte, alt, executorPluginID string, execOptions modelExecutionOptions) ([]byte, http.Header, *interfaces.ErrorMessage) {
	if h.AuthManager != nil && h.AuthManager.HomeEnabled() {
		return nil, nil, &interfaces.ErrorMessage{StatusCode: http.StatusServiceUnavailable, Error: fmt.Errorf("plugin executor routing is unavailable while Home is enabled")}
	}
	host := h.pluginExecutorHost()
	if host == nil {
		return nil, nil, &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: fmt.Errorf("plugin executor host is unavailable")}
	}
	routeInfoCollector := h.newAlwaysRouteInfoCollector(nil, false, modelName)
	req, opts := h.pluginExecutorRequest(ctx, handlerType, handlerType, modelName, originalRequestedModel, rawJSON, alt, false, execOptions)
	lifecycle := h.newRequestLifecycleTracker(ctx, handlerType, modelName, originalRequestedModel, false, opts.Metadata, execOptions.SkipInterceptorPluginID)
	if routeInfoCollector != nil {
		opts.Metadata[coreexecutor.RouteInfoReporterMetadataKey] = routeInfoCollector
	}
	var interceptErr *interfaces.ErrorMessage
	req, opts, interceptErr = h.applyRequestInterceptorsBeforeAuth(ctx, handlerType, originalRequestedModel, lifecycle.requestID(), req, opts, execOptions.SkipInterceptorPluginID)
	if interceptErr != nil {
		setOpenRouterRouteError(interceptErr, routeInfoCollector, ctx)
		lifecycle.completeError(ctx, interceptErr)
		return nil, nil, interceptErr
	}
	req, opts, interceptErr = h.applyRequestInterceptorsAfterPluginExecutorRoute(ctx, host, executorPluginID, handlerType, originalRequestedModel, lifecycle.requestID(), req, opts, execOptions.SkipInterceptorPluginID)
	if interceptErr != nil {
		setOpenRouterRouteError(interceptErr, routeInfoCollector, ctx)
		lifecycle.completeError(ctx, interceptErr)
		return nil, nil, interceptErr
	}
	ctx = enrichContextWithSessionHierarchy(ctx, opts.Headers, req.Payload, opts.Metadata)
	resp, errCount := host.CountPluginExecutor(ctx, executorPluginID, req, opts)
	if errCount != nil {
		routeInfoCollector.logFallbackIfAny()
		errMsg := executionErrorMessage(errCount)
		setOpenRouterRouteError(errMsg, routeInfoCollector, ctx)
		lifecycle.completeError(ctx, errMsg)
		return nil, nil, errMsg
	}
	rawResponseHeaders := cloneHeader(resp.Headers)
	responseHeaders := downstreamHeadersFromExecutor(rawResponseHeaders, PassthroughHeadersEnabled(h.Cfg))
	body, responseHeaders := h.applyResponseInterceptors(ctx, lifecycle.requestID(), handlerType, modelName, originalRequestedModel, opts, rawResponseHeaders, responseHeaders, opts.OriginalRequest, req.Payload, resp.Payload, http.StatusOK, execOptions.SkipInterceptorPluginID)
	responseHeaders = attachOpenRouterRouteToHeaders(responseHeaders, routeInfoCollector, ctx)
	responseHeaders = attachOpenRouterFallback(responseHeaders, routeInfoCollector)
	routeInfoCollector.logFallbackIfAny()
	// Count-token responses stay byte-for-byte unchanged (see the note on the
	// AuthManager count path).
	lifecycle.complete(pluginapi.RequestCompletionSucceeded, http.StatusOK, nil)
	return body, responseHeaders, nil
}

func (h *BaseAPIHandler) pluginExecutorRequest(ctx context.Context, entryProtocol, responseProtocol, modelName, originalRequestedModel string, rawJSON []byte, alt string, stream bool, execOptions modelExecutionOptions) (coreexecutor.Request, coreexecutor.Options) {
	reqMeta := requestExecutionMetadata(ctx)
	reqMeta[coreexecutor.RequestedModelMetadataKey] = originalRequestedModel
	addAuthSelectionModelMetadata(reqMeta, execOptions.AuthSelectionModel)
	addModelExecutionSourceMetadata(reqMeta, execOptions.InternalSource)
	setReasoningEffortMetadata(reqMeta, entryProtocol, modelName, rawJSON)
	setServiceTierMetadata(reqMeta, rawJSON)
	setGenerateMetadata(reqMeta, rawJSON)
	payload := rawJSON
	if len(payload) == 0 {
		payload = nil
	}
	req := coreexecutor.Request{Model: modelName, Payload: payload}
	opts := coreexecutor.Options{
		Stream:                    stream,
		Alt:                       alt,
		OriginalRequest:           rawJSON,
		SourceFormat:              sdktranslator.FromString(entryProtocol),
		ResponseFormat:            sdktranslator.FromString(responseProtocol),
		Headers:                   modelExecutionHeaders(ctx, execOptions.Headers),
		Query:                     modelExecutionQuery(ctx, execOptions.Query),
		WebSocketResponseObserver: h.webSocketResponseObserver("", execOptions.SkipInterceptorPluginID),
		Metadata:                  reqMeta,
	}
	return req, opts
}

func (h *BaseAPIHandler) applyRequestInterceptorsAfterPluginExecutorRoute(ctx context.Context, host PluginExecutorHost, executorPluginID, entryProtocol, originalRequestedModel, requestID string, req coreexecutor.Request, opts coreexecutor.Options, skipPluginID string) (coreexecutor.Request, coreexecutor.Options, *interfaces.ErrorMessage) {
	if !requestInterceptorsEnabled(h.interceptorHost()) {
		return req, opts, nil
	}
	toFormat := sdktranslator.FromString(entryProtocol)
	if resolver, ok := host.(pluginExecutorFormatResolver); ok && resolver != nil {
		if resolved := resolver.PluginExecutorRequestToFormat(executorPluginID, req, opts); resolved != "" {
			toFormat = resolved
		}
	}
	resp := h.applyRequestInterceptorsAfterAuth(ctx, coreexecutor.RequestAfterAuthInterceptRequest{
		SourceFormat:   opts.SourceFormat,
		ToFormat:       toFormat,
		Model:          req.Model,
		RequestedModel: originalRequestedModel,
		Stream:         opts.Stream,
		Headers:        cloneHeader(opts.Headers),
		Body:           cloneBytes(req.Payload),
		Metadata:       opts.Metadata,
	}, requestID, skipPluginID)
	opts.Headers = mergeRequestInterceptorHeaders(opts.Headers, resp.Headers, resp.ClearHeaders)
	if len(resp.Body) > 0 {
		req.Payload = cloneBytes(resp.Body)
		opts.OriginalRequest = cloneBytes(resp.Body)
	}
	if resp.Terminate {
		return req, opts, directTerminationError(resp.StatusCode, resp.ResponseHeaders, resp.ResponseBody)
	}
	return req, opts, nil
}

func ExecutionErrorMessage(err error) *interfaces.ErrorMessage {
	return executionErrorMessage(err)
}

func executionErrorMessage(err error) *interfaces.ErrorMessage {
	var terminated *coreexecutor.RequestTerminatedError
	if errors.As(err, &terminated) && terminated != nil {
		return &interfaces.ErrorMessage{
			StatusCode:     normalizedTerminationStatus(terminated.StatusCode()),
			Error:          err,
			DirectResponse: true,
			Body:           terminated.ResponseBody(),
			Headers:        terminated.ResponseHeaders(),
		}
	}
	status := http.StatusInternalServerError
	if code := clienterror.HTTPStatusFromError(err); code > 0 {
		status = code
	}
	type directResponseError interface {
		DirectResponse() bool
		ResponseBody() []byte
	}
	var direct directResponseError
	if errors.As(err, &direct) && direct != nil && direct.DirectResponse() {
		body := direct.ResponseBody()
		var headers http.Header
		if len(body) > 0 {
			contentType := http.DetectContentType(body)
			if json.Valid(body) {
				contentType = "application/json"
			}
			headers = http.Header{"Content-Type": []string{contentType}}
		}
		return &interfaces.ErrorMessage{
			StatusCode:     status,
			Error:          err,
			DirectResponse: true,
			Body:           body,
			Headers:        headers,
		}
	}
	var addon http.Header
	if he, ok := err.(interface{ Headers() http.Header }); ok && he != nil {
		if hdr := he.Headers(); hdr != nil {
			addon = hdr.Clone()
		}
	}
	return &interfaces.ErrorMessage{StatusCode: status, Error: err, Addon: addon}
}

func (h *BaseAPIHandler) pluginExecutorHost() PluginExecutorHost {
	if h == nil {
		return nil
	}
	if executorHost, ok := h.ModelRouterHost.(PluginExecutorHost); ok && executorHost != nil {
		return executorHost
	}
	if executorHost, ok := h.PluginHost.(PluginExecutorHost); ok && executorHost != nil {
		return executorHost
	}
	return nil
}
