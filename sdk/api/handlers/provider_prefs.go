package handlers

import (
	"context"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/fallback"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/providerprefs"
)

// ProviderPrefsLatencySource is the gateway's latency source for request-level
// provider preferences (RIC-3352 option A: in-process sliding window; option B,
// a KEEPER percentile puller, will implement the same seam). It returns a
// provider's in-window p99 latency in milliseconds and whether the source has
// enough data to answer it. Nil disables latency ranking entirely: a latency
// sort then degrades to the static order and is recorded as
// latency_source_empty in routing_trace.
type ProviderPrefsLatencySource interface {
	P99(provider string) (ms int64, ok bool)
}

// ProviderPrefsHeaders are the compact response headers that carry the applied /
// ignored preference record on streaming responses, where the routing_trace
// body contract is unavailable after the first chunk (docs/routing-trace.md
// Scope). They are absent when no preference object was parsed.
const (
	// ProviderPrefsAppliedHeader is a comma-joined list of the preference
	// fields that took effect (e.g. "sort:price,max_price,order").
	ProviderPrefsAppliedHeader = "X-Provider-Prefs-Applied"
	// ProviderPrefsIgnoredHeader is a comma-joined list of ignored fields as
	// "field=reason" pairs (e.g. "only:openai=not_in_pool").
	ProviderPrefsIgnoredHeader = "X-Provider-Prefs-Ignored"
)

// providerPrefsRecord is one request's resolved provider preferences: the
// pool to execute, the fallback engine's request-level options, the applied /
// ignored observability lists, and a boundary violation (403/503) when the
// KEEPER allow-list forbids the request's selection. A nil record means the
// request carried no provider object: every field is the zero value and the
// pool is the original (zero-regression path, AC5).
type providerPrefsRecord struct {
	pool       []string
	engineOpts fallback.RequestOptions
	applied    []providerprefs.Field
	ignored    []providerprefs.Field
	violation  *interfaces.ErrorMessage
	hasPrefs   bool
}

// applyProviderPreferences resolves the request-level provider object against
// an ALREADY-boundary-narrowed provider pool. It is called after
// applyRouteBoundary, so the KEEPER allow-list has already stripped out-of-
// boundary providers: the preference transform can only narrow the pool
// further and never widen it — the issue's security-priority invariant holds
// by construction (a client preference cannot push a candidate outside the
// allow-list because that candidate is not in the pool it operates on).
//
// A request body without a provider object returns a nil record, so the
// caller keeps the pool and the configured fallback chain untouched (AC5).
//
// The record's violation is non-nil only when the KEEPER boundary is in force
// and the preference narrowed the in-boundary pool to empty; the gate decides
// 403 vs 503 from the client-named providers so the body and headers match
// the enforcer's own verdicts exactly (no fourth semantics is invented, per
// the issue's rabbit-hole 2).
func (h *BaseAPIHandler) applyProviderPreferences(
	ctx context.Context,
	providers []string,
	rawJSON []byte,
	modelName string,
) *providerPrefsRecord {
	pref := providerprefs.Parse(rawJSON)
	if pref == nil {
		return nil
	}

	var costValues []providerprefs.CostValue
	var latencyValues []providerprefs.LatencyValue
	if h != nil {
		costValues = h.providerCostValues(providers)
		latencyValues = h.providerLatencyValues(providers)
	}

	// allowList is nil here on purpose: the pool is already boundary-narrowed,
	// so the preference must not re-admit out-of-boundary providers. Pass nil
	// so the transform's hard gate is a no-op and it narrows in place.
	rec := providerprefs.Apply(pref, providers, nil, costValues, latencyValues, len(rawJSON))

	out := &providerPrefsRecord{
		pool:     rec.Pool,
		applied:  rec.Applied,
		ignored:  rec.Ignored,
		hasPrefs: rec.HasPrefs,
		engineOpts: fallback.RequestOptions{
			Fallback:  rec.AllowFallbacks,
			CostLimit: recCostLimit(rec),
		},
	}

	if len(rec.Pool) == 0 {
		allowList, inForce := h.boundaryInForce(ctx)
		if inForce {
			out.violation = h.RouteBoundaryGate.ProviderPrefsViolation(
				ctx, providerPrefsOutside(pref, allowList), modelName)
		}
	}
	return out
}

// recCostLimit converts a max_price-derived cost ceiling into the
// fallback.RequestOptions form (nil when the preference set no positive
// limit, so the configured chain limit applies).
func recCostLimit(rec *providerprefs.Record) *float64 {
	if rec == nil || rec.CostLimitUSD <= 0 {
		return nil
	}
	v := rec.CostLimitUSD
	return &v
}

// boundaryInForce reports whether the KEEPER route-boundary channel is in
// force for this caller. It delegates to the gate so the handler never
// re-derives the allow-list.
func (h *BaseAPIHandler) boundaryInForce(ctx context.Context) ([]string, bool) {
	if h == nil || h.RouteBoundaryGate == nil {
		return nil, false
	}
	return h.RouteBoundaryGate.BoundaryFor(ctx)
}

// providerPrefsOutside returns the client-named providers (the provider
// object's only/order entries) that the KEEPER allow-list does not contain.
// It is the 403-vs-503 input for ProviderPrefsViolation: a non-empty result
// is an explicit out-of-boundary selection (403); empty means the pool was
// narrowed to nothing for a non-explicit reason (scenario-2 503).
func providerPrefsOutside(pref *providerprefs.Preference, allowList []string) []string {
	if pref == nil || len(allowList) == 0 {
		return nil
	}
	allowed := make(map[string]struct{}, len(allowList))
	for _, p := range allowList {
		allowed[providerPrefsNormalize(p)] = struct{}{}
	}
	requested := make([]string, 0, len(pref.Only)+len(pref.Order))
	requested = append(requested, pref.Only...)
	requested = append(requested, pref.Order...)
	var outside []string
	for _, p := range requested {
		key := providerPrefsNormalize(p)
		if key == "" {
			continue
		}
		if _, in := allowed[key]; !in {
			outside = append(outside, key)
		}
	}
	return outside
}

// providerCostValues surfaces the configured per-request cost estimates of the
// active provider pool so a sort:"price" has data to rank on. It reads the
// fallback bridge's declarative config (the operator's known cost table).
func (h *BaseAPIHandler) providerCostValues(pool []string) []providerprefs.CostValue {
	if h == nil || h.FallbackBridge == nil || len(pool) == 0 {
		return nil
	}
	pools := make(map[string]struct{}, len(pool))
	for _, p := range pool {
		pools[providerPrefsNormalize(p)] = struct{}{}
	}
	var out []providerprefs.CostValue
	for _, ref := range h.FallbackBridge.config.Providers {
		key := providerPrefsNormalize(ref.Provider)
		if key == "" {
			continue
		}
		if _, ok := pools[key]; !ok {
			continue
		}
		out = append(out, providerprefs.CostValue{Provider: key, CostUSD: ref.CostUSD})
	}
	return out
}

// providerLatencyValues reads the in-process latency window for the active
// pool. A nil source (default build, channel off) yields no values, so a
// latency sort degrades and is recorded, per the decision doc.
func (h *BaseAPIHandler) providerLatencyValues(pool []string) []providerprefs.LatencyValue {
	if h == nil || h.ProviderPrefsLatency == nil || len(pool) == 0 {
		return nil
	}
	var out []providerprefs.LatencyValue
	for _, p := range pool {
		key := providerPrefsNormalize(p)
		if key == "" {
			continue
		}
		if ms, ok := h.ProviderPrefsLatency.P99(key); ok {
			out = append(out, providerprefs.LatencyValue{Provider: key, LatencyMS: ms})
		}
	}
	return out
}

func providerPrefsNormalize(p string) string { return strings.ToLower(strings.TrimSpace(p)) }

// attachProviderPrefsHeaders stamps the compact applied/ignored preference
// headers on a response header map, for streaming responses where the
// routing_trace body is unavailable. No-op when there is nothing to record.
func attachProviderPrefsHeaders(headers http.Header, rec *providerPrefsRecord) {
	if rec == nil || !rec.hasPrefs {
		return
	}
	if headers == nil {
		return
	}
	if len(rec.applied) > 0 {
		parts := make([]string, 0, len(rec.applied))
		for _, f := range rec.applied {
			parts = append(parts, f.Name)
		}
		headers.Set(ProviderPrefsAppliedHeader, strings.Join(parts, ","))
	}
	if len(rec.ignored) > 0 {
		parts := make([]string, 0, len(rec.ignored))
		for _, f := range rec.ignored {
			if f.Reason != "" {
				parts = append(parts, f.Name+"="+f.Reason)
			} else {
				parts = append(parts, f.Name)
			}
		}
		headers.Set(ProviderPrefsIgnoredHeader, strings.Join(parts, ","))
	}
}
