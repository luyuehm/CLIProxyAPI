// Package fallback implements the automatic provider fallback orchestrator
// (OPENROUT-001). A request selects the first eligible provider from an
// ordered candidate chain; when that provider fails, the orchestrator
// automatically switches to the next candidate while the request payload
// (conversation context) is preserved. Every attempt is bounded by the
// user's declared cost, quality, and timeout constraints, and every
// fallback decision is recorded as a structured log and decision record.
//
// The package is deliberately a leaf: it is a pure policy engine that takes
// an ExecuteFunc and returns a structured outcome. It does not import the
// HTTP handlers, the executor layer, or the config package; wiring it into
// the gateway request path is a follow-up task (see the parent issue's
// NEXT_STEP).
package fallback

import (
	"errors"
	"time"
)

// Candidate is one provider slot in the ordered fallback chain.
// Policy.Candidates[0] is the preferred provider.
type Candidate struct {
	// Provider is the provider key the gateway can execute.
	Provider string
	// Model is an optional per-candidate model override. Empty means the
	// requested model is kept (fallback only crosses compatible models,
	// per the issue's known rabbit-hole rule).
	Model string
	// Quality is an estimated quality index for this candidate. 0 means
	// "unknown"; unknown estimates are never excluded by the quality floor.
	Quality float64
	// CostUSD is an estimated per-request cost in USD. 0 means "unknown";
	// an unknown estimate is admitted under a cost limit.
	CostUSD float64
}

// Policy carries the declarative strategy fields the orchestrator enforces.
// All fields are optional: a zero Policy with candidates is a plain
// failover chain with no constraints.
type Policy struct {
	// Candidates is the ordered chain; [0] is the preferred provider.
	Candidates []Candidate
	// QualityFloor excludes candidates whose known quality estimate is
	// below this value. 0 disables the floor.
	QualityFloor float64
	// CostLimit is a hard per-request USD ceiling. 0 disables it.
	// Candidates whose known estimated cost exceeds the limit are excluded
	// before any execution; if every candidate's known cost exceeds the
	// limit, Run returns ErrCostLimitExceeded without making any call.
	CostLimit float64
	// TimeoutBudget caps total wall time across all attempts. 0 means the
	// caller's context deadline is the only budget.
	TimeoutBudget time.Duration
}

// RequestOptions carries per-request switches.
type RequestOptions struct {
	// Fallback is the request-level switch. nil follows the global
	// chain; false attempts only the preferred candidate and returns its
	// error as-is, even when the policy configures a full fallback chain.
	Fallback *bool
}

// FallbackEnabled reports the effective request-level switch. A nil switch
// means "enabled" (follow the global chain).
func (o RequestOptions) FallbackEnabled() bool {
	return o.Fallback == nil || *o.Fallback
}

// Sentinel errors carry stable codes for logs, metrics, and status
// responses.
var (
	// ErrNoCandidates: the policy names no usable candidate.
	ErrNoCandidates = errors.New("fallback: no_candidates")
	// ErrCostLimitExceeded: every candidate's known estimated cost exceeds
	// Policy.CostLimit, so no provider call may be made.
	ErrCostLimitExceeded = errors.New("fallback: cost_limit_exceeded")
	// ErrQualityFloorUnsat: every candidate's known quality estimate is
	// below Policy.QualityFloor.
	ErrQualityFloorUnsat = errors.New("fallback: quality_floor_unsatisfiable")
	// ErrAllProvidersCooling: the health backoff has cooled down every
	// candidate in the chain.
	ErrAllProvidersCooling = errors.New("fallback: all_providers_cooling")
	// ErrTimeoutBudgetExhausted: the total wall time reached
	// Policy.TimeoutBudget before the next attempt could start.
	ErrTimeoutBudgetExhausted = errors.New("fallback: timeout_budget_exhausted")
)

// eligible filters the chain by the quality floor and the cost limit and
// reports which constraint (if any) made the whole chain unusable.
//
// Unknown (0) estimates are never excluded: the orchestrator cannot prove
// them out of bounds, so it lets them through and lets the execution
// result speak.
func (p Policy) eligible() ([]Candidate, error) {
	if len(p.Candidates) == 0 {
		return nil, ErrNoCandidates
	}
	out := make([]Candidate, 0, len(p.Candidates))
	seen := make(map[string]bool, len(p.Candidates))
	sawCandidate, costExceeded := false, false
	for _, c := range p.Candidates {
		if c.Provider == "" || seen[c.Provider] {
			continue
		}
		seen[c.Provider] = true
		sawCandidate = true
		if p.QualityFloor > 0 && c.Quality > 0 && c.Quality < p.QualityFloor {
			continue
		}
		if p.CostLimit > 0 && c.CostUSD > 0 && c.CostUSD > p.CostLimit {
			costExceeded = true
			continue
		}
		out = append(out, c)
	}
	if len(out) > 0 {
		return out, nil
	}
	if !sawCandidate {
		return nil, ErrNoCandidates
	}
	// The chain is unusable; report the dominant hard constraint.
	if costExceeded {
		return nil, ErrCostLimitExceeded
	}
	return nil, ErrQualityFloorUnsat
}
