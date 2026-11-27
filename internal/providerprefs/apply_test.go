package providerprefs

import (
	"reflect"
	"testing"
)

func wantPrefsJSON(t *testing.T, body []byte) *Preference {
	t.Helper()
	p := Parse(body)
	if p == nil {
		t.Fatalf("Parse returned nil; expected a preference from %q", body)
	}
	return p
}

func TestParseAbsent(t *testing.T) {
	cases := [][]byte{
		[]byte(`{"model":"gpt-5"}`),
		[]byte(``),
		nil,
	}
	for _, body := range cases {
		if p := Parse(body); p != nil {
			t.Fatalf("Parse(%s) = %v; want nil (no provider object)", body, p)
		}
	}
}

func TestParseNonObjectProviderIsRecordedNotErred(t *testing.T) {
	p := Parse([]byte(`{"model":"m","provider":"cheap"}`))
	if p == nil || !p.Present {
		t.Fatalf("provider present as a string must still be recorded, got %+v", p)
	}
	if len(p.UnknownFields) == 0 {
		t.Fatalf("non-object provider must be recorded as ignored, got %+v", p)
	}
}

func TestParseAllFields(t *testing.T) {
	body := []byte(`{
	  "model":"m",
	  "provider":{
	    "order":["openai","anthropic"],
	    "only":["anthropic","openai"],
	    "ignore":["vertex"],
	    "allow_fallbacks":false,
	    "sort":"price",
	    "max_price":{"prompt":1,"completion":2},
	    "preferred_max_latency":{"p99":0.5,"p50":0.2},
	    "preferred_min_throughput":4,
	    "require_parameters":true,
	    "data_collection":"deny",
	    "some_future_key":1
	  }
	}`)
	p := wantPrefsJSON(t, body)

	if !reflect.DeepEqual(p.Only, []string{"anthropic", "openai"}) {
		t.Errorf("Only = %v", p.Only)
	}
	if !reflect.DeepEqual(p.Ignore, []string{"vertex"}) {
		t.Errorf("Ignore = %v", p.Ignore)
	}
	if !reflect.DeepEqual(p.Order, []string{"openai", "anthropic"}) {
		t.Errorf("Order = %v", p.Order)
	}
	if p.AllowFallbacks == nil || *p.AllowFallbacks != false {
		t.Errorf("AllowFallbacks = %v; want &false", p.AllowFallbacks)
	}
	if p.Sort != "price" {
		t.Errorf("Sort = %q; want price", p.Sort)
	}
	if p.MaxPrice == nil || !p.MaxPrice.Set || p.MaxPrice.Prompt != 1 || p.MaxPrice.Completion != 2 {
		t.Errorf("MaxPrice = %+v", p.MaxPrice)
	}
	if p.PreferredMaxLatency == nil || !p.PreferredMaxLatency.HasP99 || p.PreferredMaxLatency.P99 != 0.5 || !p.PreferredMaxLatency.HasP50 {
		t.Errorf("PreferredMaxLatency = %+v", p.PreferredMaxLatency)
	}
	if p.PreferredMinThroughput == nil || !p.PreferredMinThroughput.HasP99 || p.PreferredMinThroughput.P99 != 4 {
		// A bare number maps to p99.
		t.Errorf("PreferredMinThroughput = %+v", p.PreferredMinThroughput)
	}
	if p.RequireParameters == nil || !*p.RequireParameters {
		t.Errorf("RequireParameters = %v; want &true", p.RequireParameters)
	}
	if p.DataCollection != "deny" {
		t.Errorf("DataCollection = %q", p.DataCollection)
	}
	if !reflect.DeepEqual(p.UnknownFields, []string{"some_future_key"}) {
		t.Errorf("UnknownFields = %v; want [some_future_key]", p.UnknownFields)
	}
}

func TestParseObjectSortWithPartition(t *testing.T) {
	p := wantPrefsJSON(t, []byte(`{"provider":{"sort":{"by":"latency","partition":"model"}}}`))
	if p.By != "latency" || p.Partition != "model" {
		t.Errorf("object sort = by=%q partition=%q; want latency/model", p.By, p.Partition)
	}
}

func TestMaxPriceCostLimitUSD(t *testing.T) {
	// Frozen basis: inTokens = len/4, out = in (ratio 1.0), /1e6.
	mp := &MaxPrice{Prompt: 1, Completion: 2, Set: true}
	// bodyLen 4000 -> 1000 in + 1000 out tokens -> (1*1000 + 2*1000)/1e6 = 0.003.
	if got := mp.CostLimitUSD(4000); got != 0.003 {
		t.Errorf("CostLimitUSD(4000) = %v; want 0.003", got)
	}
	// Zero rates -> no limit.
	if zero := (&MaxPrice{Set: true}).CostLimitUSD(4000); zero != 0 {
		t.Errorf("zero-rate CostLimitUSD = %v; want 0", zero)
	}
}

func TestApplyNilPreferenceIsNoOp(t *testing.T) {
	pool := []string{"openai", "anthropic", "vertex"}
	rec := Apply(nil, pool, nil, nil, nil, 0)
	if !reflect.DeepEqual(rec.Pool, pool) {
		t.Errorf("nil pref pool = %v; want untouched %v", rec.Pool, pool)
	}
	if rec.HasPrefs {
		t.Errorf("HasPrefs = true for nil preference; want false")
	}
}

func TestApplyOnlyNarrows(t *testing.T) {
	p := &Preference{Present: true, Only: []string{"anthropic"}}
	rec := Apply(p, []string{"openai", "anthropic", "vertex"}, nil, nil, nil, 0)
	if !reflect.DeepEqual(rec.Pool, []string{"anthropic"}) {
		t.Errorf("only pool = %v; want [anthropic]", rec.Pool)
	}
}

func TestApplyOnlyCannotWiden(t *testing.T) {
	// "google" is requested in only but is not a candidate -> it is recorded,
	// not added. The pool must not gain a provider.
	p := &Preference{Present: true, Only: []string{"google", "openai"}}
	rec := Apply(p, []string{"openai", "anthropic"}, nil, nil, nil, 0)
	if !reflect.DeepEqual(rec.Pool, []string{"openai"}) {
		t.Errorf("only pool = %v; want [openai] (google not widened in)", rec.Pool)
	}
	var sawNotInPool bool
	for _, f := range rec.Ignored {
		if f.Name == "only:google" && f.Reason == ReasonNotInPool {
			sawNotInPool = true
		}
	}
	if !sawNotInPool {
		t.Errorf("only:google must be recorded ignored, got %+v", rec.Ignored)
	}
}

func TestApplyOrderReorders(t *testing.T) {
	p := &Preference{Present: true, Order: []string{"vertex", "openai"}}
	rec := Apply(p, []string{"openai", "anthropic", "vertex"}, nil, nil, nil, 0)
	want := []string{"vertex", "openai", "anthropic"}
	if !reflect.DeepEqual(rec.Pool, want) {
		t.Errorf("order pool = %v; want %v", rec.Pool, want)
	}
}

func TestApplySortPriceKnownFirst(t *testing.T) {
	p := &Preference{Present: true, Sort: "price"}
	costs := []CostValue{{Provider: "openai", CostUSD: 0.01}, {Provider: "anthropic", CostUSD: 0.005}}
	rec := Apply(p, []string{"openai", "anthropic", "vertex"}, nil, costs, nil, 0)
	// anthropic (0.005) < openai (0.01), vertex unknown -> last.
	want := []string{"anthropic", "openai", "vertex"}
	if !reflect.DeepEqual(rec.Pool, want) {
		t.Errorf("price sort pool = %v; want %v", rec.Pool, want)
	}
}

func TestApplySortLatencyNoDataIsIgnored(t *testing.T) {
	p := &Preference{Present: true, Sort: "latency"}
	rec := Apply(p, []string{"openai", "anthropic"}, nil, nil, nil, 0)
	if !reflect.DeepEqual(rec.Pool, []string{"openai", "anthropic"}) {
		t.Errorf("no-data latency sort must not reorder, got %v", rec.Pool)
	}
	var saw bool
	for _, f := range rec.Ignored {
		if f.Name == "sort:latency" && f.Reason == ReasonNoLatencyData {
			saw = true
		}
	}
	if !saw {
		t.Errorf("latency no-data must be recorded ignored, got %+v", rec.Ignored)
	}
}

func TestApplyThroughputSortIgnored(t *testing.T) {
	p := &Preference{Present: true, Sort: "throughput"}
	rec := Apply(p, []string{"openai", "anthropic"}, nil, nil, nil, 0)
	var saw bool
	for _, f := range rec.Ignored {
		if f.Name == "sort:throughput" && f.Reason == ReasonThroughputSourceUnavailable {
			saw = true
		}
	}
	if !saw {
		t.Errorf("throughput sort must be recorded ignored, got %+v", rec.Ignored)
	}
	if !reflect.DeepEqual(rec.Pool, []string{"openai", "anthropic"}) {
		t.Errorf("throughput sort must not reorder, got %v", rec.Pool)
	}
}

func TestApplyPartitionNoneIgnored(t *testing.T) {
	p := &Preference{Present: true, By: "price", Partition: "none"}
	rec := Apply(p, []string{"openai", "anthropic"}, nil, []CostValue{{Provider: "anthropic", CostUSD: 1}}, nil, 0)
	var saw bool
	for _, f := range rec.Ignored {
		if f.Name == "sort:price" && f.Reason == ReasonPartitionNoneUnsupported {
			saw = true
		}
	}
	if !saw {
		t.Errorf("partition:none must be recorded ignored, got %+v", rec.Ignored)
	}
}

func TestApplyBoundaryStaysTopPriority(t *testing.T) {
	// KEEPER allow-list = [anthropic]. A client requests only:[openai] — the
	// boundary must win: openai is not in the allow-list, so the pool is
	// narrowed to [] (an empty intersection), never widened to openai. The
	// enforcer's 403/503 is what surfaces this; here we assert the narrow.
	p := &Preference{Present: true, Only: []string{"openai"}}
	rec := Apply(p, []string{"openai", "anthropic"}, []string{"anthropic"}, nil, nil, 0)
	// only:openai is not a candidate within [openai,anthropic]? openai IS a
	// candidate. only -> [openai]. Then boundary gate [anthropic] removes it.
	if len(rec.Pool) != 0 {
		t.Errorf("boundary must strip out-of-list providers, got %v", rec.Pool)
	}
}

func TestApplyBoundaryStripsOutOfListCandidates(t *testing.T) {
	p := &Preference{Present: true, Order: []string{"openai", "anthropic", "vertex"}}
	rec := Apply(p, []string{"openai", "anthropic", "vertex"}, []string{"anthropic", "vertex"}, nil, nil, 0)
	if !reflect.DeepEqual(rec.Pool, []string{"anthropic", "vertex"}) {
		t.Errorf("boundary pool = %v; want [anthropic vertex]", rec.Pool)
	}
}

func TestApplyMaxPriceSetsCostLimit(t *testing.T) {
	p := &Preference{Present: true, MaxPrice: &MaxPrice{Prompt: 1, Set: true}}
	rec := Apply(p, []string{"openai"}, nil, nil, nil, 4000)
	if rec.CostLimitUSD != 0.001 { // (1*1000)/1e6
		t.Errorf("CostLimitUSD = %v; want 0.001", rec.CostLimitUSD)
	}
}

func TestApplyUnknownFieldsIgnored(t *testing.T) {
	p := &Preference{Present: true, UnknownFields: []string{"future"}}
	rec := Apply(p, []string{"openai"}, nil, nil, nil, 0)
	var saw bool
	for _, f := range rec.Ignored {
		if f.Reason == ReasonUnknownField+":future" {
			saw = true
		}
	}
	if !saw {
		t.Errorf("unknown field must be recorded ignored, got %+v", rec.Ignored)
	}
	// Request still succeeds: the pool is returned, not an error.
	if !reflect.DeepEqual(rec.Pool, []string{"openai"}) {
		t.Errorf("unknown field must not break the request, pool=%v", rec.Pool)
	}
}

func TestApplyAllowFallbacksFalseRecorded(t *testing.T) {
	p := &Preference{Present: true}
	falseVal := false
	p.AllowFallbacks = &falseVal
	rec := Apply(p, []string{"openai", "anthropic"}, nil, nil, nil, 0)
	if rec.AllowFallbacks == nil || *rec.AllowFallbacks != false {
		t.Errorf("AllowFallbacks = %v; want &false", rec.AllowFallbacks)
	}
	var saw bool
	for _, f := range rec.Applied {
		if f.Name == "allow_fallbacks" {
			saw = true
		}
	}
	if !saw {
		t.Errorf("allow_fallbacks must be recorded applied, got %+v", rec.Applied)
	}
}
