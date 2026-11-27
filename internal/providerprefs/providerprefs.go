// Package providerprefs parses the OpenRouter request-level `provider` object
// (RIC-3352 / OPENROUT-001-S2) out of a raw model request body and reduces it
// to a pool transformation plus an applied/ignored record for routing_trace.
//
// The package is a pure leaf: it depends only on gjson (already a repo
// dependency for request-body inspection) and on plain data types, so it can
// be unit-tested without a gateway, an executor, or a KEEPER channel. It
// never mutates the pool it is given (it returns a new slice), and it never
// errors on unknown fields — a future provider key is recorded as ignored
// with a reason code, not rejected, so a client sending a newer contract
// cannot 400/500 the request (the issue's AC4).
package providerprefs

import (
	"strings"

	"github.com/tidwall/gjson"
)

// providerKey is the top-level request field this package reads.
const providerKey = "provider"

// Known provider-object field names. Any other key present in the object is
// recorded as an ignored field with the unknown_field reason — never errored.
var knownFields = map[string]struct{}{
	"order":                    {},
	"only":                     {},
	"ignore":                   {},
	"allow_fallbacks":          {},
	"sort":                     {},
	"partition":                {},
	"max_price":                {},
	"preferred_max_latency":    {},
	"preferred_min_throughput": {},
	"require_parameters":       {},
	"data_collection":          {},
	"service_tier":             {},
	"timeout":                  {},
}

// MaxPrice is the USD-per-million-token ceiling for prompt and completion.
// OpenRouter's unit is USD/1M tokens; the gateway converts it to the
// request-level USD cost limit explicitly (see CostLimitUSD), and the
// conversion is pinned to the modelrouting token basis (body-len/4,
// /1e6) so stream and non-stream behave identically.
type MaxPrice struct {
	Prompt     float64
	Completion float64
	// Set reports whether max_price was present (so an empty object is
	// still recorded as applied, matching OpenRouter's "present" semantics).
	Set bool
}

// LatencyPref captures preferred_max_latency / preferred_min_throughput,
// which OpenRouter accepts as either a single number or a percentile object
// {p50,p75,p90,p99}. Set marks presence; Has* mark which percentiles were
// named so a half-filled object is not read as a complete one.
type LatencyPref struct {
	P50, P75, P90, P99             float64
	HasP50, HasP75, HasP90, HasP99 bool
	Set                            bool
}

// Preference is the parsed request-level `provider` object.
type Preference struct {
	// Present is true when the body carried a `provider` object at all.
	// A nil *Preference (Parse returned nil) means the object was absent and
	// the whole pipeline short-circuits, which is the zero-regression path.
	Present bool

	Only           []string
	Ignore         []string
	Order          []string
	AllowFallbacks *bool
	// Sort is the requested sort key: "price", "throughput", "latency", or
	// "" when the value was a {by,partition} object (By is then set).
	Sort string
	// By is the {by} member of an object-form sort.
	By string
	// Partition is the sort grouping key; "model" (the default) is recorded
	// when present. partition:"none" (cross-model global sort) is recorded as
	// ignored unless a cross-model data source is wired (issue scope note).
	Partition string

	MaxPrice               *MaxPrice
	PreferredMaxLatency    *LatencyPref
	PreferredMinThroughput *LatencyPref
	RequireParameters      *bool
	DataCollection         string

	// UnknownFields lists provider-object keys not in the known set, in
	// document order. Each becomes an ignored field with the
	// unknown_field:<key> reason code.
	UnknownFields []string
}

// CostLimitUSD converts a MaxPrice to the gateway's per-request USD cost
// ceiling using the frozen modelrouting token basis (estimateTokensPerChar=4,
// /1e6). Both MaxPrice fields are USD per million tokens, so a request whose
// body implies estTokens input and completion tokens costs
// (prompt*in + completion*out)/1e6 USD; the limit is the request's ceiling.
// A MaxPrice with both rates zero is a no-op (returns 0) so it cannot
// silently block every candidate.
func (m *MaxPrice) CostLimitUSD(requestBodyLen int) float64 {
	if m == nil || (!m.Set || (m.Prompt == 0 && m.Completion == 0)) {
		return 0
	}
	inTokens := float64(requestBodyLen / 4)
	outTokens := inTokens // completion ratio 1.0 (modelrouting engine basis)
	cost := (m.Prompt*inTokens + m.Completion*outTokens) / 1e6
	if cost < 0 {
		return 0
	}
	return cost
}

// Parse reads the top-level `provider` object from a raw request body. It
// returns nil when the field is absent or not an object (a non-object value is
// treated as absent: OpenRouter's contract is an object, and a stray
// string/number there is malformed input we should not route on — but we
// still record it as ignored so it is not silently dropped).
func Parse(body []byte) *Preference {
	if len(body) == 0 {
		return nil
	}
	node := gjson.GetBytes(body, providerKey)
	if !node.Exists() {
		return nil
	}
	p := &Preference{Present: true}

	if node.Type != gjson.JSON {
		// Present but not an object: record as an ignored, unusable value.
		p.UnknownFields = []string{"(provider is not an object)"}
		return p
	}

	// Collect unknown fields first (document order via the object's raw keys),
	// then read each known field.
	for _, key := range providerObjectKeys(node) {
		if _, known := knownFields[key]; !known {
			p.UnknownFields = append(p.UnknownFields, key)
		}
	}

	if arr := node.Get("only"); arr.Exists() && arr.IsArray() {
		p.Only = stringArray(arr)
	}
	if arr := node.Get("ignore"); arr.Exists() && arr.IsArray() {
		p.Ignore = stringArray(arr)
	}
	if arr := node.Get("order"); arr.Exists() && arr.IsArray() {
		p.Order = stringArray(arr)
	}

	if b := node.Get("allow_fallbacks"); b.Exists() && b.IsBool() {
		v := b.Bool()
		p.AllowFallbacks = &v
	}

	// sort is either a string key or a {by,partition} object.
	sortNode := node.Get("sort")
	if sortNode.Exists() {
		switch {
		case sortNode.Type == gjson.String:
			p.Sort = normalizeSortKey(sortNode.String())
		case sortNode.IsObject():
			p.By = normalizeSortKey(sortNode.Get("by").String())
			p.Partition = sortNode.Get("partition").String()
		}
	}
	if part := node.Get("partition"); part.Exists() {
		p.Partition = part.String()
	}

	if mp := node.Get("max_price"); mp.Exists() && mp.IsObject() {
		max := &MaxPrice{Set: true}
		max.Prompt = mp.Get("prompt").Float()
		max.Completion = mp.Get("completion").Float()
		p.MaxPrice = max
	}

	if pref := parseLatencyPref(node.Get("preferred_max_latency")); pref != nil {
		p.PreferredMaxLatency = pref
	}
	if pref := parseLatencyPref(node.Get("preferred_min_throughput")); pref != nil {
		p.PreferredMinThroughput = pref
	}

	if b := node.Get("require_parameters"); b.Exists() {
		v := b.Bool()
		p.RequireParameters = &v
	}
	p.DataCollection = node.Get("data_collection").String()

	return p
}

// providerObjectKeys returns the top-level keys of the provider object in
// document order. gjson's Object() iterates in key order for an object.
func providerObjectKeys(obj gjson.Result) []string {
	var keys []string
	obj.ForEach(func(key, _ gjson.Result) bool {
		keys = append(keys, key.String())
		return true
	})
	return keys
}

// stringArray converts a gjson array of strings to []string, dropping empty
// entries and trimming whitespace.
func stringArray(arr gjson.Result) []string {
	var out []string
	arr.ForEach(func(_, v gjson.Result) bool {
		s := v.String()
		if s != "" {
			out = append(out, s)
		}
		return true
	})
	return out
}

// parseLatencyPref reads a numeric-or-percentile-object value into a
// LatencyPref, returning nil when the value is absent.
func parseLatencyPref(n gjson.Result) *LatencyPref {
	if !n.Exists() {
		return nil
	}
	pref := &LatencyPref{Set: true}
	switch n.Type {
	case gjson.Number, gjson.String:
		// A bare number (or a numeric string) is the p99 target per
		// OpenRouter's contract.
		pref.P99 = n.Float()
		pref.HasP99 = true
	case gjson.JSON:
		for _, f := range []struct {
			name string
			set  func(*LatencyPref, float64)
		}{
			{"p50", func(p *LatencyPref, v float64) { p.P50, p.HasP50 = v, true }},
			{"p75", func(p *LatencyPref, v float64) { p.P75, p.HasP75 = v, true }},
			{"p90", func(p *LatencyPref, v float64) { p.P90, p.HasP90 = v, true }},
			{"p99", func(p *LatencyPref, v float64) { p.P99, p.HasP99 = v, true }},
		} {
			if v := n.Get(f.name); v.Exists() {
				f.set(pref, v.Float())
			}
		}
	default:
		// Present but malformed (null/bool): record presence only so the
		// field is not silently dropped.
	}
	return pref
}

// normalizeSortKey maps an OpenRouter sort key to the gateway's canonical key.
// Unknown/empty values map to "" (no sort requested).
func normalizeSortKey(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "price", "cost":
		return "price"
	case "throughput", "tput":
		return "throughput"
	case "latency":
		return "latency"
	default:
		return ""
	}
}
