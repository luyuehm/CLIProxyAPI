// Package modelrouting implements the request-level model-routing strategy
// layer (parent ticket RIC-2520, Stage 1-A):
//
//   - a per-user/org routing strategy schema with strict field validation
//     (strategy.go),
//   - an immutable per-scope version history with a built-in default
//     fallback (version.go),
//   - a daily-synced, concurrency-safe model price cache fed by the KEEPER
//     price source (price_cache.go).
//
// Design rules, in priority order:
//
//  1. Routing always has an output. Every scope resolves to a strategy: the
//     newest configured version when one exists, else the built-in default.
//     A missing configuration never silences routing.
//  2. Audit by immutability. Each strategy change appends one immutable
//     version record (monotonic version, strictly increasing timestamp, full
//     snapshot, change note); stored records are never mutated.
//  3. Single price source. Prices come from KEEPER's ModelPriceSetting table
//     (served at GET /api/v1/pricing, same control-plane channel and
//     management-key convention as internal/arbitrage and internal/policies).
//     No second source is introduced.
//  4. No torn reads. The price table is swapped as one immutable
//     generation; a concurrent reader always observes a complete snapshot.
package modelrouting

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

// Optimization goals a routing strategy may declare. The Stage 2 decision
// engine (RIC-2524) interprets them; this package only validates and stores
// them.
const (
	OptMinCost     = "min_cost"
	OptBalanced    = "balanced"
	OptHighQuality = "high_quality"
)

// DefaultModel is the built-in fallback model served when no strategy is
// configured for a scope. The Stage 2 decision engine reports it in its
// x-or-reason decision text (the "chose_<model>" format frozen by the
// parent ticket), so the name stays a bare model identifier.
const DefaultModel = "claude-haiku"

// Scope kinds a strategy table may be keyed by.
const (
	ScopeUser = "user"
	ScopeOrg  = "org"
)

// Scope selects the strategy table a lookup runs against: one strategy per
// user or per organization.
type Scope struct {
	// Kind is ScopeUser or ScopeOrg.
	Kind string
	// ID is the user or organization identifier (non-empty).
	ID string
}

// Strategy is the request-level routing policy a user or organization
// declares. The JSON field names are a frozen contract consumed by the
// management API and the Stage 2 decision engine.
type Strategy struct {
	// Optimization is one of OptMinCost, OptBalanced, OptHighQuality.
	Optimization string `json:"optimization"`
	// QualityFloor is the minimum heuristic quality score, in [0,1], a
	// routed model must meet.
	QualityFloor float64 `json:"quality_floor"`
	// MaxBudget is an optional hard USD cap per request. Nil means "no cap".
	MaxBudget *float64 `json:"max_budget,omitempty"`
	// PreferredModel is the tie-breaker used when two candidate models cost
	// the same.
	PreferredModel string `json:"preferred_model,omitempty"`
	// OnlyAllowModel, when set, restricts routing to exactly this model.
	OnlyAllowModel string `json:"only_allow_model,omitempty"`
}

// Validate checks the invariants of s and reports the offending field by
// name. Callers must treat a returned error as fatal for the load or
// update: invalid input is rejected, never silently corrected.
func Validate(s Strategy) error {
	switch s.Optimization {
	case OptMinCost, OptBalanced, OptHighQuality:
	default:
		return fmt.Errorf("strategy: optimization %q must be one of %q, %q or %q",
			s.Optimization, OptMinCost, OptBalanced, OptHighQuality)
	}
	if math.IsNaN(s.QualityFloor) || s.QualityFloor < 0 || s.QualityFloor > 1 {
		return fmt.Errorf("strategy: quality_floor %v must be within [0,1]", s.QualityFloor)
	}
	if s.MaxBudget != nil {
		budget := *s.MaxBudget
		if math.IsNaN(budget) || math.IsInf(budget, 0) || budget <= 0 {
			return fmt.Errorf("strategy: max_budget %v must be a positive, finite USD amount", budget)
		}
	}
	return nil
}

// StrategyFromJSON decodes and validates a strategy document. JSON type
// errors are surfaced with the offending field name, and range violations
// are rejected with the same field-named errors as Validate, so a bad
// payload is never coerced into a working strategy.
func StrategyFromJSON(data []byte) (Strategy, error) {
	var s Strategy
	if strings.TrimSpace(string(data)) == "" {
		return Strategy{}, fmt.Errorf("strategy: document is empty")
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return Strategy{}, fmt.Errorf("strategy: invalid JSON: %w", err)
	}
	s.PreferredModel = strings.TrimSpace(s.PreferredModel)
	s.OnlyAllowModel = strings.TrimSpace(s.OnlyAllowModel)
	if err := Validate(s); err != nil {
		return Strategy{}, err
	}
	return s, nil
}

// DefaultStrategy is the built-in strategy in force for a scope with no
// configured version: balanced optimization, no quality floor, no budget
// cap, and the default model as tie-breaker.
func DefaultStrategy() Strategy {
	return Strategy{
		Optimization:   OptBalanced,
		PreferredModel: DefaultModel,
	}
}

// DefaultReason is the human-readable decision reason emitted when a
// request falls back to the built-in default strategy. It must stay
// non-empty: the parent ticket's "routing always has an output" guarantee
// includes an explainable default.
func DefaultReason() string {
	return "no strategy configured; using built-in default (model=" + DefaultModel +
		", optimization=" + OptBalanced + ")"
}
