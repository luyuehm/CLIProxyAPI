package providerprefs

import (
	"sort"
	"strings"
)

// Reason codes for ignored fields. They are the bounded machine-readable
// vocabulary routing_trace renders into provider_prefs_ignored; consumers can
// branch on them without parsing prose.
const (
	// ReasonUnknownField is emitted (as ReasonUnknownField+":<key>") for a
	// provider-object key not in the known set.
	ReasonUnknownField = "unknown_field"
	// ReasonNotInPool is emitted when a requested provider (only:/order:) is
	// not a natural candidate for the model — it cannot be applied.
	ReasonNotInPool = "not_in_pool"
	// ReasonIgnoredProvider is emitted when a provider was requested to be
	// ignored but was not a candidate anyway (a no-op, still recorded).
	ReasonIgnoredProvider = "ignored_not_in_pool"
	// ReasonEmptyIntersection is emitted when only/ignore narrows the pool to
	// zero candidates. The enforcer's route_boundary_violation (403/503) or
	// the engine's 503 surfaces the request outcome; this record keeps the
	// preference observable in routing_trace.
	ReasonEmptyIntersection = "empty_intersection"
	// ReasonLatencySourceEmpty is emitted when a latency/throughput sort was
	// requested but no in-window sample (cold start) — the sort degrades to
	// the existing static order instead of guessing.
	ReasonLatencySourceEmpty = "latency_source_empty"
	// ReasonThroughputSourceUnavailable is emitted for a throughput sort or
	// preferred_min_throughput: no throughput data source is wired this
	// phase, so the field is recorded, not silently dropped.
	ReasonThroughputSourceUnavailable = "throughput_source_unavailable"
	// ReasonPartitionNoneUnsupported is emitted for partition:"none" (the
	// cross-model global sort), which needs a cross-model data source the
	// gateway does not wire this phase.
	ReasonPartitionNoneUnsupported = "partition_none_unsupported"
	// ReasonNoCostData is emitted when sort:"price" was requested but no
	// candidate carries a known cost, so the price sort is a no-op.
	ReasonNoCostData = "no_cost_data"
	// ReasonNoLatencyData mirrors ReasonNoCostData for sort:"latency".
	ReasonNoLatencyData = "no_latency_data"
)

// Field is one preference field and whether it took effect. It is the unit
// of the applied/ignored lists rendered into routing_trace.
type Field struct {
	// Name is the OpenRouter field key ("order", "only", "max_price", …).
	Name string
	// Applied is true when the field took effect on the pool / engine.
	Applied bool
	// Reason is an empty string for an applied field, or a bounded reason
	// code (+ detail) for an ignored one.
	Reason string
}

// Record is the outcome of applying one request-level provider object to a
// pool. It drives both the routing_trace provider_prefs_* fields and the
// headers, and the pool it returns is what the boundary gate + fallback
// engine execute against.
type Record struct {
	// Pool is the transformed provider pool (never mutates the input).
	Pool []string
	// CostLimitUSD is the request-level USD per-request ceiling derived from
	// max_price (0 disables). Threaded to the fallback engine as a hard
	// candidate filter.
	CostLimitUSD float64
	// AllowFallbacks, when set, disables the fallback chain (preferred only)
	// for this request.
	AllowFallbacks *bool
	// Applied lists the fields that took effect.
	Applied []Field
	// Ignored lists the fields (and sub-causes) that did not, with reasons.
	Ignored []Field
	// HasPrefs reports whether the object carried any usable preference, so
	// a no-prefs request short-circuits and leaves the pool untouched.
	HasPrefs bool
}

// CostValue pairs a provider with its known per-request USD estimate. An
// estimate of 0 means "unknown" (never excluded), mirroring the fallback
// engine's Candidate.CostUSD semantics.
type CostValue struct {
	Provider string
	CostUSD  float64
}

// LatencyValue pairs a provider with its in-window latency percentile (ms).
// 0 means "no sample yet".
type LatencyValue struct {
	Provider  string
	LatencyMS int64
}

// Apply reduces the provider object to a pool transform + record.
//
// pool is the resolved provider pool (post model-router). boundaryAllowed is
// the KEEPER allow-list (nil = no boundary, all pool members allowed); only
// narrows, never widens. costs supplies known per-request cost estimates for
// sort:"price"; latencies supplies in-window percentile latencies for
// sort:"latency". requestBodyLen is the raw body length, used to convert
// max_price into the request cost ceiling.
//
// The transform order is fixed by the issue's priority contract:
//
//	only/ignore narrowing  →  order reorder  →  sort (price/latency)
//
// boundaryAllowed is applied as a hard gate at the end, so a client preference
// can never push a candidate outside the KEEPER allow-list (the security
// boundary stays top priority; an empty intersection is reported, not
// silently widened).
func Apply(p *Preference, pool []string, boundaryAllowed []string, costs []CostValue, latencies []LatencyValue, requestBodyLen int) *Record {
	// Normalized pool: lowercased, deduped, original order preserved.
	base := normalize(pool)

	// No preference object at all: return the pool untouched, zero record.
	if p == nil || !p.Present {
		return &Record{Pool: base, Applied: nil, Ignored: nil, HasPrefs: false}
	}
	rec := &Record{HasPrefs: true}

	costOf := mapCosts(costs)
	latencyOf := mapLatencies(latencies)

	// 1. only / ignore narrowing (before order/sort).
	pool = applyOnlyIgnore(p, base, rec)

	// 2. order reorder: keep only requested providers that survive the
	// narrow, in the requested order, then append the rest.
	pool = applyOrder(p, pool, rec)

	// 3. sort by the requested key (stable: only reorders among candidates).
	pool = applySort(p, pool, costOf, latencyOf, rec)

	// 4. hard boundary gate: narrow to the allow-list. An empty result here is
	// a reported violation, never a silent fallback to out-of-boundary.
	pool = applyBoundary(pool, boundaryAllowed, rec)

	// 5. max_price -> request-level cost ceiling.
	if p.MaxPrice != nil && p.MaxPrice.Set {
		if limit := p.MaxPrice.CostLimitUSD(requestBodyLen); limit > 0 {
			rec.CostLimitUSD = limit
			rec.Applied = append(rec.Applied, Field{Name: "max_price"})
		} else {
			rec.Ignored = append(rec.Ignored, Field{Name: "max_price", Reason: "no_cost_limit"})
		}
	}

	// 6. allow_fallbacks: false narrows the pool to the preferred candidate
	// (the first surviving provider), which is the request-level-disable
	// semantic with or without the fallback engine (a client preference may
	// not unlock a chain the engine would otherwise walk). true is the
	// default and a no-op.
	if p.AllowFallbacks != nil {
		if *p.AllowFallbacks {
			rec.Applied = append(rec.Applied, Field{Name: "allow_fallbacks"})
		} else {
			if len(pool) > 1 {
				pool = pool[:1]
			}
			rec.Applied = append(rec.Applied, Field{Name: "allow_fallbacks"})
		}
		rec.AllowFallbacks = p.AllowFallbacks
	}

	// 7. require_parameters / data_collection / service_tier: applied no-ops
	// (recorded so they are observable, per the "不静默丢弃" rule).
	if p.RequireParameters != nil {
		rec.Applied = append(rec.Applied, Field{Name: "require_parameters"})
	}
	if p.DataCollection != "" {
		rec.Applied = append(rec.Applied, Field{Name: "data_collection"})
	}

	// 8. record unknown fields as ignored with the unknown_field reason.
	for _, key := range p.UnknownFields {
		rec.Ignored = append(rec.Ignored, Field{Name: key, Reason: ReasonUnknownField + ":" + key})
	}

	// 9. preferred_max_latency: when the in-window source has data, filter
	// out candidates whose measured p99 exceeds the requested threshold
	// (OpenRouter expresses the threshold in seconds; the source is ms, so
	// convert). Unknown candidates are admitted — the source cannot prove
	// them out of range (the cost-limit unknown-admit rule). No data
	// (cold start): the preference is recorded ignored, not applied.
	if p.PreferredMaxLatency != nil && hasAnyLatency(p.PreferredMaxLatency) {
		thresholdMS, ok := preferredMaxLatencyThresholdMS(p.PreferredMaxLatency)
		if ok && hasLatency(latencyOf) {
			kept := make([]string, 0, len(pool))
			for _, prov := range pool {
				lat, known := latencyOf[prov]
				if !known || lat <= thresholdMS {
					kept = append(kept, prov)
				}
			}
			pool = kept
			rec.Applied = append(rec.Applied, Field{Name: "preferred_max_latency"})
		} else {
			rec.Ignored = append(rec.Ignored, Field{Name: "preferred_max_latency", Reason: ReasonLatencySourceEmpty})
		}
	}
	if p.PreferredMinThroughput != nil {
		rec.Ignored = append(rec.Ignored, Field{Name: "preferred_min_throughput", Reason: ReasonThroughputSourceUnavailable})
	}

	rec.Pool = pool
	return rec
}

// applyOnlyIgnore narrows the pool: it removes ignored providers, and when
// `only` is present the pool is reduced to only∩pool (never widened — a
// requested provider that is not a natural candidate cannot be added, it is
// recorded instead). An empty result is recorded as empty_intersection; the
// enforcer's 403/503 or the engine's 503 carries the request outcome.
func applyOnlyIgnore(p *Preference, pool []string, rec *Record) []string {
	if len(p.Only) == 0 && len(p.Ignore) == 0 {
		return pool
	}
	ignoreSet := toSet(p.Ignore)
	out := make([]string, 0, len(pool))
	for _, prov := range pool {
		if _, skipped := ignoreSet[prov]; skipped {
			continue
		}
		out = append(out, prov)
	}
	if len(p.Ignore) > 0 {
		poolSet := toSet(pool)
		for _, prov := range p.Ignore {
			key := normalizeOne(prov)
			if _, ok := poolSet[key]; !ok {
				rec.Ignored = append(rec.Ignored, Field{Name: "ignore:" + key, Reason: ReasonIgnoredProvider})
			}
		}
	}
	if len(p.Only) > 0 {
		poolSet := toSet(pool)
		narrowed := make([]string, 0, len(p.Only))
		for _, req := range p.Only {
			key := normalizeOne(req)
			if key == "" {
				continue
			}
			if _, ok := poolSet[key]; ok {
				narrowed = append(narrowed, key)
			} else {
				rec.Ignored = append(rec.Ignored, Field{Name: "only:" + key, Reason: ReasonNotInPool})
			}
		}
		if len(narrowed) > 0 {
			out = dedupOrdered(narrowed)
			rec.Applied = append(rec.Applied, Field{Name: "only"})
		} else {
			out = nil
			rec.Ignored = append(rec.Ignored, Field{Name: "only", Reason: ReasonEmptyIntersection})
		}
	}
	if len(p.Ignore) > 0 && len(out) > 0 {
		rec.Applied = append(rec.Applied, Field{Name: "ignore"})
	}
	return out
}

// applyOrder reorders the pool to the requested order (requested first, in
// order, then the remainder in original order). Providers named in order but
// not in the pool are recorded as outside_boundary.
func applyOrder(p *Preference, pool []string, rec *Record) []string {
	if len(p.Order) == 0 {
		return pool
	}
	poolSet := toSet(pool)
	reordered := make([]string, 0, len(pool)+len(p.Order))
	seen := make(map[string]struct{}, len(p.Order))
	for _, req := range p.Order {
		key := normalizeOne(req)
		if key == "" {
			continue
		}
		if _, ok := poolSet[key]; ok {
			reordered = append(reordered, key)
			seen[key] = struct{}{}
		} else {
			rec.Ignored = append(rec.Ignored, Field{Name: "order:" + key, Reason: ReasonNotInPool})
		}
	}
	for _, prov := range pool {
		if _, done := seen[prov]; !done {
			reordered = append(reordered, prov)
		}
	}
	if len(reordered) > 0 {
		rec.Applied = append(rec.Applied, Field{Name: "order"})
	}
	return reordered
}

// applySort reorders among the surviving pool by the requested key. It is a
// stable refinement: candidates with unknown values keep their relative
// order, and a sort with no data is a recorded no-op.
func applySort(p *Preference, pool []string, costOf map[string]float64, latencyOf map[string]int64, rec *Record) []string {
	key := p.Sort
	if key == "" {
		key = p.By
	}
	if key == "" {
		return pool
	}
	// partition:"none" needs a cross-model data source; record, don't apply.
	if p.Partition == "none" {
		rec.Ignored = append(rec.Ignored, Field{Name: "sort:" + key, Reason: ReasonPartitionNoneUnsupported})
		return pool
	}
	out := make([]string, len(pool))
	copy(out, pool)

	switch key {
	case "price":
		if !anyKnown(costOf) {
			rec.Ignored = append(rec.Ignored, Field{Name: "sort:price", Reason: ReasonNoCostData})
			return pool
		}
		sortKnownFirst(out, costOf, func(a, b float64) bool { return a < b })
		rec.Applied = append(rec.Applied, Field{Name: "sort:price"})
	case "latency":
		if !anyLatency(latencyOf) {
			rec.Ignored = append(rec.Ignored, Field{Name: "sort:latency", Reason: ReasonNoLatencyData})
			return pool
		}
		sortKnownFirstInt(out, latencyOf, func(a, b int64) bool { return a < b })
		rec.Applied = append(rec.Applied, Field{Name: "sort:latency"})
	case "throughput":
		// No throughput source this phase: record, leave order intact.
		rec.Ignored = append(rec.Ignored, Field{Name: "sort:throughput", Reason: ReasonThroughputSourceUnavailable})
	}
	return out
}

// sortKnownFirst stably sorts list so providers with a known, lower value
// come first, and providers with an unknown (zero) value keep their relative
// order at the back. lower(a,b) reports whether value a is better than b
// among known values.
func sortKnownFirst(list []string, costOf map[string]float64, better func(a, b float64) bool) {
	sort.SliceStable(list, func(i, j int) bool {
		av, ai := costOf[list[i]]
		bv, bi := costOf[list[j]]
		if ai == bi {
			// Same known-ness: rank by the known value (both unknown keeps order).
			if !ai {
				return false
			}
			return better(av, bv)
		}
		// Known beats unknown; among two known values, the better one wins.
		if ai && !bi {
			return true
		}
		if bi && !ai {
			return false
		}
		return better(av, bv)
	})
}

// sortKnownFirstInt is the int64 variant of sortKnownFirst.
func sortKnownFirstInt(list []string, latOf map[string]int64, better func(a, b int64) bool) {
	sort.SliceStable(list, func(i, j int) bool {
		av, ai := latOf[list[i]]
		bv, bi := latOf[list[j]]
		if ai == bi {
			if !ai {
				return false
			}
			return better(av, bv)
		}
		if ai && !bi {
			return true
		}
		if bi && !ai {
			return false
		}
		return better(av, bv)
	})
}

// applyBoundary is the final hard gate: keep only pool members that pass the
// KEEPER allow-list. boundaryAllowed nil/empty = no constraint.
func applyBoundary(pool []string, boundaryAllowed []string, rec *Record) []string {
	if len(boundaryAllowed) == 0 {
		return pool
	}
	allow := toSet(boundaryAllowed)
	out := make([]string, 0, len(pool))
	for _, prov := range pool {
		if _, ok := allow[prov]; ok {
			out = append(out, prov)
		}
	}
	return out
}

// mapCosts / mapLatencies build provider->value lookup maps.
func mapCosts(costs []CostValue) map[string]float64 {
	if len(costs) == 0 {
		return nil
	}
	m := make(map[string]float64, len(costs))
	for _, c := range costs {
		m[normalizeOne(c.Provider)] = c.CostUSD
	}
	return m
}

func mapLatencies(latencies []LatencyValue) map[string]int64 {
	if len(latencies) == 0 {
		return nil
	}
	m := make(map[string]int64, len(latencies))
	for _, l := range latencies {
		m[normalizeOne(l.Provider)] = l.LatencyMS
	}
	return m
}

func anyKnown(m map[string]float64) bool {
	for _, v := range m {
		if v > 0 {
			return true
		}
	}
	return false
}

func anyLatency(m map[string]int64) bool {
	for _, v := range m {
		if v > 0 {
			return true
		}
	}
	return false
}

func hasLatency(m map[string]int64) bool { return m != nil && anyLatency(m) }

func hasAnyLatency(p *LatencyPref) bool {
	return p != nil && (p.HasP50 || p.HasP75 || p.HasP90 || p.HasP99)
}

// preferredMaxLatencyThresholdMS converts a request's preferred_max_latency
// to a millisecond threshold the in-window p99 source can compare against.
// OpenRouter expresses the value in seconds; the source is ms, so multiply by
// 1000. It picks the highest set percentile (p99 > p90 > p75 > p50) as the
// bound: that is the loosest the client is willing to accept, so filtering
// candidates whose measured p99 exceeds it never admits one the client
// rejected. ok=false when no percentile value was named.
func preferredMaxLatencyThresholdMS(p *LatencyPref) (int64, bool) {
	if p == nil {
		return 0, false
	}
	value, found := 0.0, false
	for _, s := range []struct {
		has *bool
		val *float64
	}{
		{&p.HasP99, &p.P99},
		{&p.HasP90, &p.P90},
		{&p.HasP75, &p.P75},
		{&p.HasP50, &p.P50},
	} {
		if *s.has {
			value, found = *s.val, true
			break
		}
	}
	if !found {
		return 0, false
	}
	return int64(value * 1000), true
}

// dedupOrdered returns a de-duplicated copy of list preserving first-seen
// order.
func dedupOrdered(list []string) []string {
	seen := make(map[string]struct{}, len(list))
	out := make([]string, 0, len(list))
	for _, s := range list {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// normalize lowercases + trims + dedupes a provider pool, preserving order.
func normalize(pool []string) []string {
	seen := make(map[string]struct{}, len(pool))
	out := make([]string, 0, len(pool))
	for _, p := range pool {
		key := normalizeOne(p)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	return out
}

func normalizeOne(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func toSet(list []string) map[string]struct{} {
	if len(list) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(list))
	for _, s := range list {
		set[normalizeOne(s)] = struct{}{}
	}
	return set
}
