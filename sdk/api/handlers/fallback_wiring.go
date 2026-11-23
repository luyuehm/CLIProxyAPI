package handlers

import (
	"context"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// executeWithFallback runs a non-streaming request through the fallback
// bridge when one is installed, otherwise through the AuthManager directly.
// It returns the response, the per-request fallback outcome state (inactive
// when no bridge is installed or no chain was walked), and the execution
// error.
func (h *BaseAPIHandler) executeWithFallback(
	ctx context.Context,
	providers []string,
	req coreexecutor.Request,
	opts coreexecutor.Options,
) (coreexecutor.Response, *fallbackOutcomeState, error) {
	state := &fallbackOutcomeState{}
	if h == nil || h.FallbackBridge == nil {
		resp, err := h.authManager().Execute(ctx, providers, req, opts)
		return resp, state, err
	}
	underlying := func(c context.Context, p []string, r coreexecutor.Request, o coreexecutor.Options) (coreexecutor.Response, error) {
		return h.authManager().Execute(c, p, r, o)
	}
	resp, attempt, err := h.FallbackBridge.execute(ctx, providers, req, opts, underlying)
	state.set(attempt)
	return resp, state, err
}

// executeCountWithFallback runs a non-streaming count request through the
// fallback bridge when one is installed, otherwise through the AuthManager
// directly.
func (h *BaseAPIHandler) executeCountWithFallback(
	ctx context.Context,
	providers []string,
	req coreexecutor.Request,
	opts coreexecutor.Options,
) (coreexecutor.Response, *fallbackOutcomeState, error) {
	state := &fallbackOutcomeState{}
	if h == nil || h.FallbackBridge == nil {
		resp, err := h.authManager().ExecuteCount(ctx, providers, req, opts)
		return resp, state, err
	}
	underlying := func(c context.Context, p []string, r coreexecutor.Request, o coreexecutor.Options) (coreexecutor.Response, error) {
		return h.authManager().ExecuteCount(c, p, r, o)
	}
	resp, attempt, err := h.FallbackBridge.execute(ctx, providers, req, opts, underlying)
	state.set(attempt)
	return resp, state, err
}

// executeStreamWithFallback runs a streaming request through the fallback
// bridge when one is installed, otherwise through the AuthManager directly.
func (h *BaseAPIHandler) executeStreamWithFallback(
	ctx context.Context,
	providers []string,
	req coreexecutor.Request,
	opts coreexecutor.Options,
) (*coreexecutor.StreamResult, *fallbackOutcomeState, error) {
	state := &fallbackOutcomeState{}
	if h == nil || h.FallbackBridge == nil {
		result, err := h.authManager().ExecuteStream(ctx, providers, req, opts)
		return result, state, err
	}
	underlying := func(c context.Context, p []string, r coreexecutor.Request, o coreexecutor.Options) (*coreexecutor.StreamResult, error) {
		return h.authManager().ExecuteStream(c, p, r, o)
	}
	result, attempt, err := h.FallbackBridge.executeStream(ctx, providers, req, opts, underlying)
	state.set(attempt)
	return result, state, err
}

// authManager returns the AuthManager. The handlers always have a manager in
// production; this guards the nil path.
func (h *BaseAPIHandler) authManager() *coreauth.Manager {
	if h == nil {
		return nil
	}
	return h.AuthManager
}
