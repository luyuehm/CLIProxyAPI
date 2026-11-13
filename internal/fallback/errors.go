package fallback

import (
	"errors"
	"fmt"
	"time"
)

// ProviderError describes one failed upstream attempt. Executions wrap their
// provider-level failures in this type so the orchestrator can emit a stable
// machine-readable reason code in logs and FallbackRecords.
type ProviderError struct {
	// Provider is the provider key that produced the failure.
	Provider string
	// Status is the HTTP status of the upstream response, 0 when the
	// failure was transport-level (no response at all).
	Status int
	// Err is the underlying error, if any.
	Err error
}

// Reason is the stable code for structured logging, e.g. "provider_A_503".
// The provider key is kept verbatim; status 0 reports a transport error.
func (e *ProviderError) Reason() string {
	if e == nil {
		return "provider_error"
	}
	if e.Status != 0 {
		return fmt.Sprintf("provider_%s_%d", e.Provider, e.Status)
	}
	return "provider_" + e.Provider + "_error"
}

// Error implements error.
func (e *ProviderError) Error() string {
	code := e.Reason()
	if e.Err != nil {
		return code + ": " + e.Err.Error()
	}
	return code
}

// Unwrap returns the underlying error.
func (e *ProviderError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// coolingError is a health-gate skip: the provider is inside its backoff
// cooldown. It is not a sentinel: the terminal all-cooling case is
// ErrAllProvidersCooling, while this type carries the per-provider skip so
// switch records get a stable machine code.
type coolingError struct {
	provider string
	until    time.Time
}

// Reason is the stable code for structured logging, e.g.
// "provider_A_cooled_down".
func (e *coolingError) Reason() string {
	return "provider_" + e.provider + "_cooled_down"
}

// Error implements error.
func (e *coolingError) Error() string {
	return fmt.Sprintf("%s until %s", e.Reason(), e.until.Format(time.RFC3339))
}

// reasonCode maps an attempt error to a bounded log/metric code built from
// the provider key and status only. It deliberately never returns raw error
// text: net/http error strings carry full URLs, and Gemini-style keys ride
// in query strings, so raw text in a "stable" code would leak credential
// material into metrics labels and API responses. Unwrap only known types;
// anything else degrades to a generic per-provider code.
func reasonCode(err error, provider string) string {
	if err == nil {
		return "success"
	}
	var pe *ProviderError
	if errors.As(err, &pe) {
		return pe.Reason()
	}
	var ce *coolingError
	if errors.As(err, &ce) {
		return ce.Reason()
	}
	// Sentinel errors carry their stable code without the "fallback:"
	// prefix, so metric labels stay snake_case like the rest.
	if errors.Is(err, ErrNoCandidates) {
		return "no_candidates"
	}
	if errors.Is(err, ErrCostLimitExceeded) {
		return "cost_limit_exceeded"
	}
	if errors.Is(err, ErrQualityFloorUnsat) {
		return "quality_floor_unsatisfiable"
	}
	if errors.Is(err, ErrAllProvidersCooling) {
		return "all_providers_cooling"
	}
	if errors.Is(err, ErrTimeoutBudgetExhausted) {
		return "timeout_budget_exhausted"
	}
	if provider != "" {
		return "provider_" + provider + "_error"
	}
	return "attempt_error"
}
