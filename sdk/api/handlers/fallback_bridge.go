package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/fallback"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// FallbackReasonHeader is the always-on response header that reports the
// bounded machine-readable reason a request was automatically degraded to a
// backup provider (RIC-3143, OPENROUT-001). It is absent when no provider
// fallback occurred: a direct success on the preferred provider, or a request
// whose provider chain was never exercised.
//
// The value is an engine reason code, e.g. "provider_openai_503",
// "provider_openai_cooled_down", "timeout_budget_exhausted", or
// "provider_openai_error" (transport-level). It is complementary to
// X-OpenRouter-Fallback (the model pair): this header names WHY the request
// degraded, the model pair names WHAT moved.
const FallbackReasonHeader = "X-Fallback-Reason"

// fallbackReasonHeaderMaxLen bounds the X-Fallback-Reason value so a verbose
// reason cannot exceed HTTP header size limits.
const fallbackReasonHeaderMaxLen = 200

// fallbackBridge executes one request through the internal/fallback engine:
// it walks the configured provider chain, and for each provider calls the
// underlying execution path exactly as the gateway would without the engine.
// It is a pure adapter — the engine's Policy/Health semantics are unchanged.
//
// A nil fallbackBridge (the default when no fallback block is configured)
// makes every method pass through to the underlying executor with no engine
// overhead, so the gateway behavior is identical to before.
//
// The bridge carries no per-request state: every Run returns its Outcome and
// the caller threads it through the request for observability. The engine and
// its HealthWindow are shared across concurrent requests and are
// concurrency-safe.
type fallbackBridge struct {
	// orchestrator is the engine instance, shared across concurrent requests.
	orchestrator *fallback.Orchestrator
	// config is the declarative strategy, retained so the bridge can build
	// the Policy for a request (the chain is per-request: the preferred
	// provider varies with the model).
	config fallback.Config
}

// newFallbackBridge builds a bridge from the declarative config. A disabled
// or empty config returns a nil bridge (no engine overhead).
func newFallbackBridge(cfg fallback.Config, enabled bool) (*fallbackBridge, error) {
	if !enabled || len(cfg.Providers) == 0 {
		return nil, nil
	}
	policy, health, err := cfg.ToPolicy()
	if err != nil {
		return nil, err
	}
	o := fallback.NewOrchestrator(policy, health)
	return &fallbackBridge{
		orchestrator: o,
		config:       cfg,
	}, nil
}

// fallbackAttempt describes one run of the engine for one request.
// Ran reports whether the engine actually walked a chain of two or more
// providers: a request with a single-provider chain passes through the
// underlying executor untouched and keeps today's error semantics, so its
// outcome must not be read as a fallback decision.
type fallbackAttempt struct {
	Ran     bool
	Outcome fallback.Outcome
}

// execute runs one non-streaming request through the engine. Each engine
// attempt calls the underlying executor with a single-provider pool, so the
// engine controls provider-level failover while the auth conductor retains its
// credential-level failover inside each provider. The request payload and
// context are preserved across attempts (only the provider pool narrows).
//
// On success it returns the serving response; on terminal failure it returns
// the engine error. Either way the returned attempt describes what the engine
// did so the caller can render X-Fallback-Reason and attempted_models.
func (b *fallbackBridge) execute(
	ctx context.Context,
	providers []string,
	req coreexecutor.Request,
	opts coreexecutor.Options,
	underlying func(context.Context, []string, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error),
) (coreexecutor.Response, fallbackAttempt, error) {
	if b == nil {
		resp, err := underlying(ctx, providers, req, opts)
		return resp, fallbackAttempt{}, err
	}
	policy := b.policyFor(providers)
	if len(policy.Candidates) < 2 {
		// No fallback chain to walk: run directly so a single-provider
		// failure keeps its native status and message.
		resp, err := underlying(ctx, providers, req, opts)
		return resp, fallbackAttempt{}, err
	}
	var attemptResp coreexecutor.Response
	out := b.orchestrator.Run(ctx, req.Model, fallback.RequestOptions{}, func(attemptCtx context.Context, provider, model string) error {
		attemptReq := req
		attemptReq.Model = model
		attemptOpts := cloneCoreOptions(opts)
		resp, err := underlying(attemptCtx, []string{provider}, attemptReq, attemptOpts)
		if err != nil {
			return fallbackProviderError(provider, err)
		}
		attemptResp = resp
		return nil
	})
	if !out.Success {
		if out.Err != nil {
			return coreexecutor.Response{}, fallbackAttempt{Ran: true, Outcome: out}, out.Err
		}
		return coreexecutor.Response{}, fallbackAttempt{Ran: true, Outcome: out}, fmt.Errorf("fallback: chain exhausted")
	}
	return attemptResp, fallbackAttempt{Ran: true, Outcome: out}, nil
}

// executeStream runs one streaming request through the engine. The engine
// treats each provider attempt as a full stream establishment: a stream that
// fails to establish on a provider falls back to the next provider, while a
// stream that starts is the final result (mid-stream failures do not migrate
// — the engine is a provider-level failover, not a stream relay).
func (b *fallbackBridge) executeStream(
	ctx context.Context,
	providers []string,
	req coreexecutor.Request,
	opts coreexecutor.Options,
	underlying func(context.Context, []string, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error),
) (*coreexecutor.StreamResult, fallbackAttempt, error) {
	if b == nil {
		result, err := underlying(ctx, providers, req, opts)
		return result, fallbackAttempt{}, err
	}
	policy := b.policyFor(providers)
	if len(policy.Candidates) < 2 {
		result, err := underlying(ctx, providers, req, opts)
		return result, fallbackAttempt{}, err
	}
	var attemptStream *coreexecutor.StreamResult
	out := b.orchestrator.Run(ctx, req.Model, fallback.RequestOptions{}, func(attemptCtx context.Context, provider, model string) error {
		attemptReq := req
		attemptReq.Model = model
		attemptOpts := cloneCoreOptions(opts)
		result, err := underlying(attemptCtx, []string{provider}, attemptReq, attemptOpts)
		if err != nil {
			return fallbackProviderError(provider, err)
		}
		attemptStream = result
		return nil
	})
	if !out.Success {
		if out.Err != nil {
			return nil, fallbackAttempt{Ran: true, Outcome: out}, out.Err
		}
		return nil, fallbackAttempt{Ran: true, Outcome: out}, fmt.Errorf("fallback: chain exhausted")
	}
	return attemptStream, fallbackAttempt{Ran: true, Outcome: out}, nil
}

// policyFor builds the engine policy for one request. The configured chain is
// used when present; otherwise the resolved provider pool order is used with
// no model overrides or constraints. When the configured chain is present but
// the resolved pool is narrower (e.g. a KEEPER route boundary), the chain is
// intersected with the pool so fallback never leaves the pool.
func (b *fallbackBridge) policyFor(providers []string) fallback.Policy {
	pool := normalizeProviderPool(providers)
	cfg := b.config
	if len(cfg.Providers) == 0 {
		p := fallback.Policy{}
		for _, provider := range pool {
			p.Candidates = append(p.Candidates, fallback.Candidate{Provider: provider})
		}
		return p
	}
	p := fallback.Policy{
		QualityFloor: cfg.QualityFloor,
		CostLimit:    cfg.CostLimitUSD,
	}
	if b.orchestrator != nil && b.orchestrator.Policy.TimeoutBudget > 0 {
		p.TimeoutBudget = b.orchestrator.Policy.TimeoutBudget
	}
	poolSet := make(map[string]struct{}, len(pool))
	for _, provider := range pool {
		poolSet[provider] = struct{}{}
	}
	for _, ref := range cfg.Providers {
		provider := normalizeProvider(ref.Provider)
		if provider == "" {
			continue
		}
		// A KEEPER route boundary may have narrowed the pool; the fallback
		// chain must never step outside what the boundary allowed.
		if len(poolSet) > 0 {
			if _, ok := poolSet[provider]; !ok {
				continue
			}
		}
		p.Candidates = append(p.Candidates, fallback.Candidate{
			Provider: provider,
			Model:    strings.TrimSpace(ref.Model),
			Quality:  ref.Quality,
			CostUSD:  ref.CostUSD,
		})
	}
	if len(p.Candidates) == 0 {
		// The configured chain did not survive the pool intersection; fall
		// back to the resolved pool so the request still executes.
		for _, provider := range pool {
			p.Candidates = append(p.Candidates, fallback.Candidate{Provider: provider})
		}
	}
	return p
}

// normalizeProviderPool lowercases and deduplicates a provider pool.
func normalizeProviderPool(providers []string) []string {
	seen := make(map[string]struct{}, len(providers))
	var out []string
	for _, provider := range providers {
		p := normalizeProvider(provider)
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

func normalizeProvider(provider string) string {
	return strings.TrimSpace(strings.ToLower(provider))
}

// fallbackProviderError wraps an underlying execution failure as a
// fallback.ProviderError so the engine emits a stable bounded reason code
// built from the provider key and HTTP status only.
func fallbackProviderError(provider string, err error) error {
	if err == nil {
		return nil
	}
	return &fallback.ProviderError{Provider: provider, Status: providerErrorStatus(err), Err: err}
}

// providerErrorStatus extracts an HTTP-like status from a gateway error.
func providerErrorStatus(err error) int {
	type statusCoder interface{ StatusCode() int }
	var sc statusCoder
	if errors.As(err, &sc) && sc != nil {
		if code := sc.StatusCode(); code > 0 {
			return code
		}
	}
	var authErr *coreauth.Error
	if errors.As(err, &authErr) && authErr != nil && authErr.HTTPStatus > 0 {
		return authErr.HTTPStatus
	}
	return 0
}

// cloneCoreOptions copies execution options so the conductor's per-attempt
// metadata mutations (selected auth, attempted tracker) never leak across
// engine attempts and the request payload stays intact.
func cloneCoreOptions(opts coreexecutor.Options) coreexecutor.Options {
	cloned := opts
	if opts.Headers != nil {
		cloned.Headers = opts.Headers.Clone()
	}
	if opts.Query != nil {
		cloned.Query = cloneURLValues(opts.Query)
	}
	if opts.Metadata != nil {
		cloned.Metadata = cloneMetadata(opts.Metadata)
	}
	if opts.OriginalRequest != nil {
		cloned.OriginalRequest = append([]byte(nil), opts.OriginalRequest...)
	}
	return cloned
}

// cloneMetadata copies a metadata map's string keys; function values
// (callbacks) are carried by reference because they must keep the same
// callback identity.
func cloneMetadata(src map[string]any) map[string]any {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]any, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// reasonHeader renders X-Fallback-Reason for one engine outcome. It returns ""
// when the run did not degrade (a direct success on the preferred provider, or
// a run that never walked a chain), so the header stays absent on a healthy
// path and disappears again when the preferred provider recovers (AC2).
func reasonHeader(attempt fallbackAttempt) string {
	if !attempt.Ran || !attempt.Outcome.Success || attempt.Outcome.Provider == "" {
		return ""
	}
	for i := len(attempt.Outcome.Fallbacks) - 1; i >= 0; i-- {
		rec := attempt.Outcome.Fallbacks[i]
		if rec.To == attempt.Outcome.Provider && rec.From != "" && rec.From != rec.To {
			reason := fallbackHeaderSafe(rec.Reason)
			if len(reason) > fallbackReasonHeaderMaxLen {
				reason = reason[:fallbackReasonHeaderMaxLen]
			}
			return reason
		}
	}
	return ""
}

// attemptedModels renders the attempted provider chain for a terminal
// failure. It is the AC3 contract: the providers actually attempted, in
// attempt order, deduplicated.
func attemptedModels(attempt fallbackAttempt) []string {
	seen := make(map[string]struct{})
	var chain []string
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" {
			return
		}
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		chain = append(chain, p)
	}
	for _, rec := range attempt.Outcome.Fallbacks {
		add(rec.From)
		add(rec.To)
	}
	if len(chain) == 0 && attempt.Outcome.Provider != "" {
		add(attempt.Outcome.Provider)
	}
	return chain
}

// fallbackErrorMessage wraps a terminal engine outcome into an OpenAI-style
// 503 error body carrying attempted_models and the bounded reason code (AC3),
// plus the fallback observability headers.
func fallbackErrorMessage(attempt fallbackAttempt) *interfaces.ErrorMessage {
	reason := "provider_error"
	if len(attempt.Outcome.Fallbacks) > 0 {
		reason = attempt.Outcome.Fallbacks[len(attempt.Outcome.Fallbacks)-1].Reason
	}
	models := attemptedModels(attempt)
	if len(models) == 0 {
		models = []string{}
	}
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message":          fmt.Sprintf("all providers unavailable after automatic fallback (reason: %s)", reason),
			"type":             "server_error",
			"code":             "provider_unavailable",
			"attempted_models": models,
		},
	})
	headers := make(http.Header)
	if reason := reasonHeader(attempt); reason != "" {
		headers.Set(FallbackReasonHeader, reason)
	}
	return &interfaces.ErrorMessage{
		StatusCode:     http.StatusServiceUnavailable,
		Error:          fmt.Errorf("fallback: %s", reason),
		DirectResponse: true,
		Body:           body,
		Headers:        headers,
	}
}

// fallbackOutcomeState threads one request's engine outcome from execution to
// header/error rendering. It is per-request, so concurrent requests never
// share observability state.
type fallbackOutcomeState struct {
	// mu guards the attempt so a concurrent reader (the stream header
	// rendering) sees a consistent value.
	mu      sync.Mutex
	attempt fallbackAttempt
	// active reports whether the engine actually ran for this request.
	active bool
}

func (s *fallbackOutcomeState) set(attempt fallbackAttempt) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.attempt = attempt
	s.active = attempt.Ran
	s.mu.Unlock()
}

func (s *fallbackOutcomeState) snapshot() (fallbackAttempt, bool) {
	if s == nil {
		return fallbackAttempt{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempt, s.active
}

// reasonHeader renders X-Fallback-Reason for this request's outcome.
func (s *fallbackOutcomeState) reasonHeader() string {
	attempt, active := s.snapshot()
	if !active {
		return ""
	}
	return reasonHeader(attempt)
}

// buildErrorMessage renders a terminal 503 with attempted_models for this
// request's outcome. It returns nil unless the engine ran and exhausted the
// chain: a request that failed without falling back (a single-provider chain,
// a request-scoped rejection) keeps its raw error.
func (s *fallbackOutcomeState) buildErrorMessage() *interfaces.ErrorMessage {
	attempt, active := s.snapshot()
	if !active || attempt.Outcome.Success {
		return nil
	}
	return fallbackErrorMessage(attempt)
}

// attachToHeaders stamps X-Fallback-Reason onto a response header map,
// allocating it only when the outcome carries a value.
func (s *fallbackOutcomeState) attachToHeaders(headers http.Header) http.Header {
	if s == nil {
		return headers
	}
	reason := s.reasonHeader()
	if reason == "" {
		return headers
	}
	if headers == nil {
		headers = make(http.Header)
	}
	headers.Set(FallbackReasonHeader, reason)
	return headers
}

// fallbackBridgeEnabled reports whether a bridge should be constructed for the
// given config block. It is the gate the production builder checks.
func fallbackBridgeEnabled(cfg fallback.Config, enabled bool) bool {
	return enabled && len(cfg.Providers) > 0
}
