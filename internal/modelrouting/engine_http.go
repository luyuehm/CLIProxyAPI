package modelrouting

// The HTTP-facing surface of the decision engine (RIC-2524 / OPENROUT-002-S2).
// The decision core in engine.go is pure and has no I/O; this file adds:
//
//   - the explainability response headers x-or-model / x-or-reason,
//   - the failure bodies for the cost hard cap (402 budget_exceeded) and the
//     unavailable forced model (503 model_unavailable), built as
//     DirectResponse ErrorMessages so the gateway writes them verbatim,
//   - the structured, S3-replayable decision log (one Info line per
//     decision carrying request_id, strategy_version, model, est_cost,
//     quality, and reason).
//
// The error body field names follow the repository's existing OpenAI-
// compatible convention: {"error":{"code","type","message"}} — the same
// shape routeboundary's contract pins; only the code values are new
// (budget_exceeded / model_unavailable), and those two values are frozen.

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/sirupsen/logrus"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
)

// Frozen response headers. Absent when the engine is not in the path; set on
// the response before the gateway writes anything.
const (
	// HeaderModel carries the chosen model identifier.
	HeaderModel = "x-or-model"
	// HeaderReason carries the human-readable decision reason, header-safe.
	HeaderReason = "x-or-reason"
)

// Frozen error codes for the engine's two terminal failure semantics. The
// parent ticket's acceptance scenarios pin these exact values; S3 replay and
// FinOps reporting parse them.
const (
	// ErrorBudgetExceeded is the 402 body code when no candidate fits the
	// max_budget cap.
	ErrorBudgetExceeded = "budget_exceeded"
	// ErrorModelUnavailable is the 503 body code when the forced model
	// cannot be served and routing must not degrade.
	ErrorModelUnavailable = "model_unavailable"
)

// Engine is the request-facing decision engine: it owns the S1-A strategy
// scope table and price cache and turns one routing request into an
// explainable Decision. It is built via NewEngine and is safe for
// concurrent use — every request reads an immutable snapshot and the
// decision core holds no shared mutable state.
type Engine struct {
	// versions resolves the in-force strategy per scope (S1-A). Nil means
	// the built-in default strategy always applies.
	versions *ScopeTable
	// prices supplies the immutable price snapshots (S1-A). Nil means no
	// candidates are priced, so every decision degrades to the default.
	prices *PriceCache
}

// EngineOptions wires an Engine. Either component may be nil; the engine
// then degrades to the built-in default strategy with an empty candidate
// set, which is still a valid, explainable output.
type EngineOptions struct {
	// Versions is the strategy scope table; nil disables version lookups.
	Versions *ScopeTable
	// Prices is the daily price cache; nil disables priced routing.
	Prices *PriceCache
}

// NewEngine builds a request-facing engine over the given S1-A components.
func NewEngine(opts EngineOptions) *Engine {
	return &Engine{versions: opts.Versions, prices: opts.Prices}
}

// DecideForRequest resolves the caller scope's in-force strategy (S1-A
// version table, or the built-in default when unconfigured) and the current
// price snapshot, then runs the decision engine. A zero scope routes with the
// built-in default strategy (parent scenario 3). The decision's
// StrategyVersion is populated from the scope table (0 = default applied).
func (e *Engine) DecideForRequest(scope Scope, body []byte, requestedModel string) Decision {
	if e == nil {
		return Decide(DefaultStrategy(), PriceSnapshot{}, body, requestedModel, DefaultReason())
	}
	strategy, found, note := e.effective(scope)
	var snapshot PriceSnapshot
	if e.prices != nil {
		snapshot = e.prices.Snapshot()
	}
	version := int64(0)
	if found {
		if v := e.lastVersion(scope); v > 0 {
			version = v
		}
	}
	decision := Decide(strategy, snapshot, body, requestedModel, note)
	decision.StrategyVersion = version
	return decision
}

// effective resolves the in-force strategy plus the audit note that the
// ok decision's reason carries (strategy vN in force / default reason).
func (e *Engine) effective(scope Scope) (Strategy, bool, string) {
	if e.versions == nil {
		return DefaultStrategy(), false, DefaultReason()
	}
	return e.versions.Effective(scope)
}

// lastVersion returns the newest recorded version number for the scope, or 0.
func (e *Engine) lastVersion(scope Scope) int64 {
	if e.versions == nil {
		return 0
	}
	versions := e.versions.List(scope)
	if len(versions) == 0 {
		return 0
	}
	return versions[len(versions)-1].Version
}

// SetDecisionHeaders writes the explainability headers onto the response
// writer. ok decisions set both x-or-model and x-or-reason; failure
// decisions set x-or-reason only (the model is the one that could not be
// served, which the error body names).
func SetDecisionHeaders(w http.ResponseWriter, d Decision) {
	if w == nil {
		return
	}
	if d.Outcome == OutcomeOK {
		w.Header().Set(HeaderModel, d.Model)
	}
	if reason := headerSafe(d.Reason); reason != "" {
		w.Header().Set(HeaderReason, reason)
	}
}

// ErrorFor returns the terminal-failure ErrorMessage a gateway surfaces when
// the decision did not select a runnable model. ok decisions return nil. The
// budget-exceeded body is 402 {"error":{"code":"budget_exceeded"}} and the
// unavailable-model body 503 {"error":{"code":"model_unavailable"}}; both
// are DirectResponse so the gateway writes them verbatim without an
// upstream call.
func ErrorFor(d Decision) *interfaces.ErrorMessage {
	switch d.Outcome {
	case OutcomeOK:
		return nil
	case OutcomeBudgetExceeded:
		return terminalError(http.StatusPaymentRequired, ErrorBudgetExceeded,
			"no candidate model fits the configured routing budget")
	case OutcomeModelUnavailable:
		message := "forced model " + d.Model + " is not available"
		if d.Model == "" {
			message = "no eligible candidate model satisfies the routing quality floor"
		}
		return terminalError(http.StatusServiceUnavailable, ErrorModelUnavailable, message)
	default:
		return nil
	}
}

// terminalError builds a DirectResponse OpenAI-compatible error body.
func terminalError(status int, code, message string) *interfaces.ErrorMessage {
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"code":    code,
			"type":    code,
			"message": message,
		},
	})
	headers := make(http.Header)
	if reason := headerSafe("route_rejected:" + code); reason != "" {
		headers.Set(HeaderReason, reason)
	}
	return &interfaces.ErrorMessage{
		StatusCode:     status,
		DirectResponse: true,
		Body:           body,
		Headers:        headers,
		Error:          fmt.Errorf("modelrouting: route rejected (%s)", code),
	}
}

// LogDecision emits the structured, S3-replayable decision log: one Info
// line carrying the frozen field set (request_id, strategy_version, model,
// est_cost, quality, reason, outcome). requestID is supplied by the caller
// (the request-lifecycle id); strategy_version 0 marks the built-in default.
func LogDecision(requestID string, d Decision) {
	logger.WithFields(logrus.Fields{
		"request_id":       requestID,
		"strategy_version": d.StrategyVersion,
		"model":            d.Model,
		"est_cost":         d.EstCostUSD,
		"quality":          d.Quality,
		"reason":           d.Reason,
		"outcome":          d.Outcome,
	}).Info("modelrouting: decision")
}
