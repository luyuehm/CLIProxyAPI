package modelroutingruntime

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/modelrouting"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	"golang.org/x/net/context"
)

// Fused reasons reported when the gate deliberately declines to route.
const (
	// passthroughReasonNoPrices marks an installed gate with an empty price
	// cache: the request keeps its own model.
	passthroughReasonNoPrices = "route_passthrough:no_price_snapshot"
	// passthroughReasonNoCandidate marks a price cache whose every row this
	// gateway cannot serve for the request (no provider, wrong modality).
	passthroughReasonNoCandidate = "route_passthrough:no_servable_candidate"
	// passthroughReasonUnmetFloor marks a servable pool where no candidate
	// meets the requested quality tier: a statement about the price table, not
	// a violation, so the request keeps its own model.
	passthroughReasonUnmetFloor = "route_passthrough:no_model_meets_quality_floor"
)

// effectiveView is the routing view the gate applies to a scope: the strategy
// in force, whether a configured version was found, the audit reason and the
// version number (0 = no configured version).
type effectiveView struct {
	Strategy modelrouting.Strategy
	Found    bool
	Reason   string
	Version  int64
}

// RouteRequest applies the request-level model routing strategy to one
// request (handlers.ModelRoutingGate).
//
// Degradation contract (RIC-3351 rabbit holes 2 and the candidate-pool
// constraint): the gate only ever chooses a model the gateway can serve.
//
//   - No price snapshot (KEEPER unreachable, not yet synced, no keeper-url):
//     the engine's own outcome for an empty candidate set is a 503, which
//     would turn "enabled but KEEPER down" into a full outage. The request
//     passes through unchanged.
//   - A snapshot whose rows the gateway cannot serve for this request are
//     filtered out by req.Allowed before the decision runs. Without that
//     filter the engine picks the cheapest row of the whole price table and
//     could hand back an image-only model (503 from the image-only guard) or
//     a model with no credential (400 model_not_found).
//
// In both cases the request keeps its own model and x-or-reason says why.
// Routing must never be the reason a request that used to succeed fails.
func (r *Runtime) RouteRequest(ctx context.Context, req handlers.ModelRoutingRequest) handlers.ModelRoutingOutcome {
	if r == nil || r.core == nil {
		return handlers.ModelRoutingOutcome{}
	}

	scope := scopeFromContext(ctx)
	view := r.effective(scope)
	snapshot := r.core.Prices.Snapshot()

	if len(snapshot.Prices) == 0 {
		r.notePassthrough(ctx, passthroughReasonNoPrices, req.RequestedModel, scope, "no price snapshot")
		return handlers.ModelRoutingOutcome{Headers: passthroughHeaders(passthroughReasonNoPrices)}
	}

	servable := r.servableSnapshot(snapshot, req.Allowed, req.AllowedGeneration)
	if len(servable.Prices) == 0 {
		r.notePassthrough(ctx, passthroughReasonNoCandidate, req.RequestedModel, scope,
			"no priced model is servable on this endpoint")
		return handlers.ModelRoutingOutcome{Headers: passthroughHeaders(passthroughReasonNoCandidate)}
	}

	decision := modelrouting.Decide(view.Strategy, servable, req.Body, req.RequestedModel, view.Reason)
	decision.StrategyVersion = view.Version
	// A snapshot older than the staleness horizon still produces a valid
	// model, so the request is not passed through — but the reason must say
	// the inputs are old, because a max_budget decision made on a stale table
	// can be a false 402 rather than a false optimisation.
	if r.core.Prices.IsStale(time.Now(), r.core.StalenessHorizon) {
		decision.Reason = strings.TrimSpace(decision.Reason + " [stale_price_snapshot]")
	}
	modelrouting.LogDecision(requestID(ctx), decision)

	headers := make(http.Header)
	modelrouting.SetDecisionHeaders(headerOnlyWriter{headers: headers}, decision)

	outcome := handlers.ModelRoutingOutcome{Headers: headers}
	if decision.Outcome == modelrouting.OutcomeOK {
		outcome.Model = decision.Model
		return outcome
	}
	// The operator's own constraints are authoritative and must reject:
	//   - budget_exceeded: the cap is the point of the policy (an operator who
	//     set max_budget expects a 402 rather than a silent overspend);
	//   - capability_constraint_violation: the client asked for capabilities
	//     no model has.
	//
	// A model_unavailable on the general path is different. There the engine
	// simply found no priced candidate above the requested quality tier — a
	// statement about the price table, not a violation of anything anyone
	// asked for. Rejecting would 503 a complex request on a gateway whose
	// registered models are all cheap (and the shipped example config sets a
	// 0.6 floor, so this is reachable out of the box). That path passes
	// through to the requested model; the operator still sees why via the
	// header. The one pinned case — only_allow_model — keeps its 503: there the
	// operator named the model, so failing is the contract.
	if decision.Outcome == modelrouting.OutcomeModelUnavailable && reasonIsUnmetFloor(decision.Reason) {
		r.notePassthrough(ctx, passthroughReasonUnmetFloor, req.RequestedModel, scope,
			"no servable model meets the requested quality tier")
		return handlers.ModelRoutingOutcome{Headers: passthroughHeaders(passthroughReasonUnmetFloor)}
	}
	outcome.Error = modelrouting.ErrorFor(decision)
	return outcome
}

// reasonIsUnmetFloor reports whether a model_unavailable decision came from the
// general-path quality-tier filter rather than a forced-model pin.
func reasonIsUnmetFloor(reason string) bool {
	return strings.Contains(reason, "no_eligible_candidate_meets_quality_floor")
}

// servableSnapshot returns the price snapshot narrowed to the models the
// gateway can serve, reusing the previous result while both the price snapshot
// and the caller's servability generation are unchanged.
//
// The filter costs one registry read-lock per priced model. That is fine on a
// cache miss (once per price sync / credential change) but not per request, so
// the result is memoised. A zero generation means the caller cannot version its
// answer, and the snapshot is simply not cached.
func (r *Runtime) servableSnapshot(snapshot modelrouting.PriceSnapshot, allowed func(string) bool, generation uint64) modelrouting.PriceSnapshot {
	if generation == 0 {
		return filterSnapshot(snapshot, allowed)
	}

	r.servableMu.RLock()
	cached, ok := r.servable, r.servableGeneration == generation && r.servableSyncedAt.Equal(snapshot.SyncedAt)
	r.servableMu.RUnlock()
	if ok {
		return modelrouting.PriceSnapshot{Prices: cached, SyncedAt: snapshot.SyncedAt}
	}

	filtered := filterSnapshot(snapshot, allowed)
	r.servableMu.Lock()
	r.servable = filtered.Prices
	r.servableGeneration = generation
	r.servableSyncedAt = snapshot.SyncedAt
	r.servableMu.Unlock()
	return filtered
}

// filterSnapshot narrows a price snapshot to the models the gateway can serve
// for this request.
//
// A nil predicate is treated as "nothing is servable", not "no constraint".
// The failure mode of the wrong polarity is severe (a chat request routed to an
// image-only model or a credential-less model → 503/400), while declining costs
// only the optimisation. The handler always supplies one; this is defence in
// depth for any future caller.
func filterSnapshot(snapshot modelrouting.PriceSnapshot, allowed func(string) bool) modelrouting.PriceSnapshot {
	if len(snapshot.Prices) == 0 {
		return snapshot
	}
	if allowed == nil {
		return modelrouting.PriceSnapshot{SyncedAt: snapshot.SyncedAt}
	}
	filtered := make(map[string]modelrouting.Price, len(snapshot.Prices))
	for model, price := range snapshot.Prices {
		if allowed(model) {
			filtered[model] = price
		}
	}
	return modelrouting.PriceSnapshot{Prices: filtered, SyncedAt: snapshot.SyncedAt}
}

// passthroughHeaders builds the x-or-reason-only header set for a deliberate
// pass-through.
func passthroughHeaders(reason string) http.Header {
	headers := make(http.Header)
	headers.Set(modelrouting.HeaderReason, reason)
	return headers
}

// notePassthrough records a declined routing decision. The warning is emitted
// once per reason (a permanently enabled channel with a down KEEPER would
// otherwise write one warning per request for the whole deployment, drowning
// the log); later occurrences drop to Debug. The per-request signal is the
// x-or-reason header, which every request still carries.
func (r *Runtime) notePassthrough(ctx context.Context, reason, requestedModel string, scope modelrouting.Scope, detail string) {
	fields := logrus.Fields{
		"request_id":      requestID(ctx),
		"scope_kind":      scope.Kind,
		"requested_model": requestedModel,
		"reason":          reason,
	}
	// One flag per reason: a deployment can hit both the empty-cache and the
	// no-servable-candidate state, and each deserves one line.
	switch reason {
	case passthroughReasonNoPrices:
		if r.warnedNoPrices.CompareAndSwap(false, true) {
			logger.WithFields(fields).Warn("modelrouting: request passed through unchanged (" + detail + "); further occurrences log at debug")
			return
		}
	case passthroughReasonNoCandidate:
		if r.warnedNoCandidate.CompareAndSwap(false, true) {
			logger.WithFields(fields).Warn("modelrouting: request passed through unchanged (" + detail + "); further occurrences log at debug")
			return
		}
	case passthroughReasonUnmetFloor:
		if r.warnedUnmetFloor.CompareAndSwap(false, true) {
			logger.WithFields(fields).Warn("modelrouting: request passed through unchanged (" + detail + "); further occurrences log at debug")
			return
		}
	}
	logger.WithFields(fields).Debug("modelrouting: request passed through unchanged (" + detail + ")")
}

// effective resolves the strategy in force for a scope, substituting the
// configured default strategy when the scope has no appended version. Without
// a configured default the engine's built-in default applies (found=false,
// non-empty reason), so routing always has an output.
//
// Strategy and version come from ONE lookup, so a concurrent POST can never
// make a decision taken under vN be logged as v(N+1).
func (r *Runtime) effective(scope modelrouting.Scope) effectiveView {
	if latest, ok := r.core.Versions.Latest(scope); ok {
		return effectiveView{
			Strategy: latest.Strategy,
			Found:    true,
			Reason:   "strategy v" + strconv.FormatInt(latest.Version, 10) + " in force",
			Version:  latest.Version,
		}
	}
	view := r.core.Management.Effective(scope)
	out := effectiveView{Strategy: view.Strategy, Found: view.Found, Reason: view.Reason}
	if r.defaultStrategy != nil {
		out.Strategy = *r.defaultStrategy
		out.Reason = configuredDefaultReason(*r.defaultStrategy)
	}
	return out
}

// configuredDefaultReason is the audit note carried by a decision made under
// the configured (not per-scope) default strategy.
func configuredDefaultReason(strategy modelrouting.Strategy) string {
	return "configured default_strategy in force (optimization=" + strategy.Optimization + ")"
}

// requestID resolves the request-lifecycle id for the decision log, when the
// middleware chain has assigned one.
func requestID(ctx context.Context) string {
	ginCtx := ginContext(ctx)
	if ginCtx == nil {
		return ""
	}
	if id := strings.TrimSpace(logging.GetGinRequestID(ginCtx)); id != "" {
		return id
	}
	if ginCtx.Request != nil {
		return strings.TrimSpace(logging.GetRequestID(ginCtx.Request.Context()))
	}
	return ""
}

// headerOnlyWriter adapts an http.Header to the http.ResponseWriter the
// modelrouting.SetDecisionHeaders helper expects. Nothing is ever written: the
// decision headers are the only output.
type headerOnlyWriter struct {
	headers http.Header
}

func (w headerOnlyWriter) Header() http.Header { return w.headers }

func (w headerOnlyWriter) Write(p []byte) (int, error) { return len(p), nil }

func (w headerOnlyWriter) WriteHeader(int) {}
