package modelrouting

// The Stage 2 decision engine (RIC-2524 / OPENROUT-002-S2). Given a request
// body, a price snapshot, and the strategy in force, Decide picks "the
// cheapest model that satisfies the quality floor" and emits an explainable,
// auditable decision: chosen model, estimated cost, quality score, and a
// human-readable reason. It enforces the cost hard cap (402) and the
// forced-model-unavailable semantics (503) without ever reaching an upstream
// executor.
//
// This file is the pure decision core. It holds no shared mutable state, so
// two concurrent requests sharing one price snapshot never
// cross-contaminate (parent scenario 5). The HTTP-facing surface — the
// x-or-model / x-or-reason response headers, the 402/503 error bodies, and
// the structured S3 replay log — lives in engine_http.go. The engine is a
// leaf: it depends only on the S1-A strategy/price types and the S1-B
// scorer, never on the HTTP handler, so it stays trivially unit-testable.

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// Decision outcome kinds. OutcomeOK is the only kind that names a model to
// run; the others are terminal failures the gateway must surface without
// ever dispatching an upstream request.
const (
	// OutcomeOK: a model was selected; run it.
	OutcomeOK = "ok"
	// OutcomeBudgetExceeded: no eligible candidate fits the hard cap -> 402.
	OutcomeBudgetExceeded = "budget_exceeded"
	// OutcomeModelUnavailable: the forced model (or any eligible model) can
	// not be served -> 503; routing deliberately does not degrade to another
	// model.
	OutcomeModelUnavailable = "model_unavailable"
	// OutcomeConstraintViolation: no candidate model meets the request's
	// required_capabilities -> 400; the decision carries the blocked models
	// and each one's missing capabilities so the client can act on it.
	OutcomeConstraintViolation = "constraint_violation"
)

// Decision is the explainable, auditable output of the engine. Its fields are
// the frozen S3 replay contract (model, est_cost, quality, reason, outcome)
// plus the outcome kind the HTTP adapter maps to a status and error body.
type Decision struct {
	// Outcome is one of OutcomeOK, OutcomeBudgetExceeded,
	// OutcomeModelUnavailable, OutcomeConstraintViolation.
	Outcome string
	// Model is the chosen model; empty for non-ok outcomes.
	Model string
	// EstCostUSD is the estimated cost of the chosen model; 0 for non-ok.
	EstCostUSD float64
	// Quality is the request's heuristic quality score (the capability floor
	// this request needs), in [0,1].
	Quality float64
	// Reason is the human-readable, header-safe decision text. For ok
	// decisions it always carries the frozen "chose_<model>:" segment; an
	// optional audit note (e.g. the in-force strategy version text or the
	// S1-A default reason) follows it in brackets.
	Reason string
	// RequestedModel is the client-requested model, echoed for the audit log.
	RequestedModel string
	// StrategyVersion is the S1-A record number in force for this decision
	// (0 = built-in default applied). Set by the request-facing engine.
	StrategyVersion int64
	// BlockedModels lists the candidates filtered out by a
	// required_capabilities constraint, each with the capabilities it
	// lacks. Empty when the request carried no constraint or no candidate
	// was filtered.
	BlockedModels []BlockedModel `json:"blocked_models,omitempty"`
}

// BlockedModel is one candidate excluded by a capability constraint.
type BlockedModel struct {
	// Model is the candidate model identifier.
	Model string `json:"model"`
	// MissingCapabilities are the required capabilities the model lacks.
	MissingCapabilities []string `json:"missing_capabilities"`
}

// candidate is one model's precomputed decision input.
type candidate struct {
	name    string
	level   CapabilityLevel
	estCost float64
}

// estimateTokensPerChar is the frozen KISS token-estimate factor: request
// tokens are estimated from body characters with no tokenizer and no network
// (same factor quality.go uses).
const estimateTokensPerChar = 4

// defaultCompletionRatio estimates completion tokens as a multiple of prompt
// tokens. 1.0 (completion ~= prompt) is the conservative default; it keeps
// the cost estimate a single deterministic function of the body length.
const defaultCompletionRatio = 1.0

// Decide runs the decision engine: score the request, resolve the candidate
// set from the snapshot and the strategy's forced-model / quality-floor
// constraints, apply the cost cap, pick a model per the optimization goal,
// and build the explainable decision. It is pure: the only inputs are the
// body, the price snapshot, the strategy, and the requested model, so
// concurrent calls on the same snapshot are isolated.
//
// auditNote is an optional non-empty note describing the in-force strategy
// (e.g. the S1-A "strategy v3 in force" text or the built-in default reason);
// it is appended to the ok decision's reason so the audit log records which
// strategy drove the choice. An empty note is ignored.
func Decide(strategy Strategy, snapshot PriceSnapshot, body []byte, requestedModel string, auditNote string) Decision {
	score := ScoreBody(body)
	estTokens := estimatedTokens(len(body))
	requiredCapabilities := requiredCapabilitiesFromBody(body)

	// Resolve the forced-model constraint first: it is a hard pin — when set,
	// routing may only use that model and must never degrade to another
	// (user-control semantics, parent scenario 4).
	if forced := strings.TrimSpace(strategy.OnlyAllowModel); forced != "" {
		price, found := snapshot.PriceFor(forced)
		if !found {
			// Forced model is not in the price snapshot: unavailable, no
			// fallback to any other model.
			return Decision{
				Outcome:        OutcomeModelUnavailable,
				Model:          forced,
				Quality:        score.Quality,
				Reason:         "forced_model_unavailable:" + headerSafe(forced),
				RequestedModel: requestedModel,
			}
		}
		if missing := missingCapabilities(forced, requiredCapabilities); len(missing) > 0 {
			// The hard constraint outranks the forced pin: a forced model
			// that cannot meet the request's capabilities is a 400, never a
			// silent fallback to another model.
			return Decision{
				Outcome:        OutcomeConstraintViolation,
				Model:          forced,
				Quality:        score.Quality,
				Reason:         "capability_constraint_violation:" + headerSafe(forced),
				RequestedModel: requestedModel,
				BlockedModels: []BlockedModel{
					{Model: forced, MissingCapabilities: missing},
				},
			}
		}
		c := candidate{name: forced, level: levelFor(forced), estCost: estimateCost(price, estTokens)}
		return decideForced(c, strategy, score, requestedModel, auditNote)
	}

	// General path: candidate set = priced models, constrained first by the
	// request's required_capabilities (the hard boundary, applied before any
	// selection), then by the capability tier that meets the greater of the
	// strategy floor and the request's own needs.
	seen := make(map[string]struct{}, len(snapshot.Prices))
	allCands := make([]candidate, 0, len(snapshot.Prices))
	for name, p := range snapshot.Prices {
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		allCands = append(allCands, candidate{name: name, level: levelFor(name), estCost: estimateCost(p, estTokens)})
	}

	kept, blocked := filterByCapabilities(allCands, requiredCapabilities)
	if len(kept) == 0 && len(blocked) > 0 {
		// Every priced candidate violates the constraint: 400, listing each
		// blocked model and its missing capabilities. No candidate is
		// silently dropped below the constraint (scenario 2).
		return Decision{
			Outcome:        OutcomeConstraintViolation,
			Quality:        score.Quality,
			Reason:         "capability_constraint_violation",
			RequestedModel: requestedModel,
			BlockedModels:  blocked,
		}
	}

	// Capability-tier filter on the constrained pool.
	required := levelForFloor(strategy.QualityFloor)
	if rl := score.RequiredLevel(); rl > required {
		required = rl
	}
	cands := make([]candidate, 0, len(kept))
	for _, c := range kept {
		if !MeetsLevel(c.name, required) {
			continue
		}
		cands = append(cands, c)
	}
	if len(cands) == 0 {
		return Decision{
			Outcome:        OutcomeModelUnavailable,
			Quality:        score.Quality,
			Reason:         "no_eligible_candidate_meets_quality_floor",
			RequestedModel: requestedModel,
			BlockedModels:  blocked,
		}
	}

	// Cost hard cap: restrict the pool to candidates within budget. When the
	// cap is set and the pool becomes empty, every candidate exceeds it ->
	// 402 with no upstream call (parent scenario 2).
	if strategy.MaxBudget != nil {
		cap := *strategy.MaxBudget
		kept := cands[:0]
		for _, c := range cands {
			if c.estCost <= cap {
				kept = append(kept, c)
			}
		}
		if len(kept) == 0 {
			return Decision{
				Outcome:        OutcomeBudgetExceeded,
				Quality:        score.Quality,
				Reason:         "budget_exceeded:max_budget=" + formatUSD(cap),
				RequestedModel: requestedModel,
				BlockedModels:  blocked,
			}
		}
		cands = kept
	}

	chosen := pick(cands, strategy, requestedModel)
	decision := okDecision(chosen, score, requestedModel, auditNote)
	decision.BlockedModels = blocked
	return decision
}

// decideForced handles the only_allow_model pin: honor it (user control),
// still enforce the budget cap, never substitute another model.
func decideForced(c candidate, strategy Strategy, score QualityScore, requestedModel, auditNote string) Decision {
	if strategy.MaxBudget != nil && c.estCost > *strategy.MaxBudget {
		return Decision{
			Outcome:        OutcomeBudgetExceeded,
			Model:          c.name,
			Quality:        score.Quality,
			Reason:         "budget_exceeded:forced_model_over_cap",
			RequestedModel: requestedModel,
		}
	}
	return okDecision(c, score, requestedModel, auditNote)
}

// okDecision builds the success decision and its frozen reason segment.
func okDecision(c candidate, score QualityScore, requestedModel, auditNote string) Decision {
	reason := reasonFor(c.name, score.Quality, c.estCost)
	if note := strings.TrimSpace(auditNote); note != "" {
		reason += " [" + note + "]"
	}
	return Decision{
		Outcome:        OutcomeOK,
		Model:          c.name,
		EstCostUSD:     c.estCost,
		Quality:        score.Quality,
		Reason:         reason,
		RequestedModel: requestedModel,
	}
}

// pick selects a candidate by the strategy's optimization goal, applying the
// KISS tie-break rules:
//
//   - min_cost:     the cheapest; ties break to requested model, then name.
//   - balanced:     the candidate nearest the median cost (the 50th
//     percentile); ties break to the cheaper of the two middle values.
//   - high_quality: the highest capability tier; within that tier the
//     cheapest, then requested model, then name.
//
// A candidate equal to requestedModel or the strategy's preferred model wins
// a cost tie, keeping the user's declared intent intact.
func pick(cands []candidate, strategy Strategy, requestedModel string) candidate {
	requested := strings.TrimSpace(requestedModel)
	preferred := strings.TrimSpace(strategy.PreferredModel)
	lessKey := func(a, b candidate) int {
		if a.name == requested && b.name != requested {
			return -1
		}
		if b.name == requested && a.name != requested {
			return 1
		}
		if a.name == preferred && b.name != preferred {
			return -1
		}
		if b.name == preferred && a.name != preferred {
			return 1
		}
		if a.name < b.name {
			return -1
		}
		if a.name > b.name {
			return 1
		}
		return 0
	}

	switch strategy.Optimization {
	case OptHighQuality:
		byTier := make([]candidate, len(cands))
		copy(byTier, cands)
		sort.Slice(byTier, func(i, j int) bool {
			if byTier[i].level != byTier[j].level {
				return byTier[i].level > byTier[j].level
			}
			if byTier[i].estCost != byTier[j].estCost {
				return byTier[i].estCost < byTier[j].estCost
			}
			return lessKey(byTier[i], byTier[j]) < 0
		})
		return byTier[0]
	case OptBalanced:
		byCost := make([]candidate, len(cands))
		copy(byCost, cands)
		sort.Slice(byCost, func(i, j int) bool {
			if byCost[i].estCost != byCost[j].estCost {
				return byCost[i].estCost < byCost[j].estCost
			}
			return byCost[i].name < byCost[j].name
		})
		mid := len(byCost) / 2
		// Odd count: the middle element. Even count: the 50th percentile,
		// biased to the cheaper of the two middle candidates (or the
		// client-requested one when both are eligible there) — balanced must
		// still prefer spending less.
		if len(byCost)%2 == 0 {
			lo, hi := byCost[mid-1], byCost[mid]
			if hi.estCost < lo.estCost || (hi.estCost == lo.estCost && hi.name == requested) {
				return hi
			}
			return lo
		}
		return byCost[mid]
	default: // OptMinCost
		best := cands[0]
		for _, c := range cands[1:] {
			if c.estCost < best.estCost ||
				(c.estCost == best.estCost && lessKey(c, best) < 0) {
				best = c
			}
		}
		return best
	}
}

// estimatedTokens applies the frozen chars/4 token estimate.
func estimatedTokens(bodyLen int) int {
	if bodyLen < 0 {
		return 0
	}
	return bodyLen / estimateTokensPerChar
}

// estimateCost computes the USD estimate for one request: prompt tokens at
// the input rate and completion tokens (prompt * defaultCompletionRatio) at
// the output rate, both scaled by the model's multiplier and divided by 1e6
// (prices are per-1M tokens). A non-positive multiplier is treated as 1.
func estimateCost(p Price, estTokens int) float64 {
	mult := p.Multiplier
	if mult <= 0 {
		mult = 1
	}
	tok := float64(estTokens)
	cost := (tok*p.PromptPricePer1M + tok*defaultCompletionRatio*p.CompletionPricePer1M) * mult / 1e6
	if math.IsNaN(cost) || math.IsInf(cost, 0) {
		return 0
	}
	return cost
}

// levelFor returns a model's capability tier; unknown models resolve to
// LevelLite (the conservative floor), matching MeetsLevel semantics.
func levelFor(name string) CapabilityLevel {
	lvl, ok := LevelForModel(name)
	if !ok {
		return LevelLite
	}
	return lvl
}

// levelForFloor maps the strategy's [0,1] quality floor to the minimum
// capability tier using the frozen quality bands (quality.go thresholds).
func levelForFloor(floor float64) CapabilityLevel {
	switch {
	case floor <= qualityThresholdStandard:
		return LevelLite
	case floor <= qualityThresholdAdvanced:
		return LevelStandard
	case floor <= qualityThresholdFrontier:
		return LevelAdvanced
	default:
		return LevelFrontier
	}
}

// reasonFor builds the frozen reason segment:
// "chose_<model>: quality_score=<f> cost=$<f>". The S3 replay parser depends
// on this exact shape; do not reformat it.
func reasonFor(model string, quality, cost float64) string {
	return fmt.Sprintf("chose_%s: quality_score=%.2f cost=$%.4f", headerSafe(model), quality, cost)
}

// headerSafe strips non-printable ASCII so a client-supplied model name can
// never inject a newline or escape sequence into a response header.
func headerSafe(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 0x20 && r <= 0x7e {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// formatUSD renders a USD amount for reason text.
func formatUSD(v float64) string {
	return fmt.Sprintf("%.4f", v)
}
