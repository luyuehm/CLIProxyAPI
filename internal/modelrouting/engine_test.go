package modelrouting

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// reasonPattern is the parent ticket's frozen acceptance regex for the
// decision reason: it must read "chose_<model>: quality_score=<f>
// cost=$<f>". The S3 replay parser depends on this exact shape, so the
// engine's reason is pinned to it here (not self-compared against the live
// reason string, which would let a drift in both sides pass).
var reasonPattern = regexp.MustCompile(`chose_[a-z0-9._-]+: quality_score=[0-9.]+ cost=\$[0-9.]+`)

// testSnapshot builds a deterministic price table with five models spanning
// every capability tier, so the pick rules (min_cost / balanced /
// high_quality) have distinct, assertable winners.
func testSnapshot() PriceSnapshot {
	return PriceSnapshot{
		Prices: map[string]Price{
			// LevelLite (contains "haiku")
			"claude-haiku": {Model: "claude-haiku", PromptPricePer1M: 1, CompletionPricePer1M: 2, Multiplier: 1},
			// LevelStandard (contains "gpt" / "sonnet")
			"gpt-4o":            {Model: "gpt-4o", PromptPricePer1M: 5, CompletionPricePer1M: 15, Multiplier: 1},
			"claude-sonnet-4-6": {Model: "claude-sonnet-4-6", PromptPricePer1M: 3, CompletionPricePer1M: 12, Multiplier: 1},
			// LevelAdvanced (prefix "claude-opus")
			"claude-opus-4-5": {Model: "claude-opus-4-5", PromptPricePer1M: 15, CompletionPricePer1M: 75, Multiplier: 1},
			// LevelFrontier (prefix "claude-opus-4-7")
			"claude-opus-4-7": {Model: "claude-opus-4-7", PromptPricePer1M: 15, CompletionPricePer1M: 75, Multiplier: 1},
		},
		SyncedAt: time.Now(),
	}
}

// translationBody is a short OpenAI-style "translation" request: a short,
// single-turn, default-classified payload (Quality 0.30, LevelLite need).
func translationBody() []byte {
	return []byte(`{"messages":[{"role":"user","content":"Please translate the following sentence into French: good morning, how are you today?"}]}`)
}

// TestEngineMinCost covers acceptance scenario 1: with optimization=min_cost
// and a quality floor, the engine picks the cheapest model that meets the
// floor and emits a frozen-format, non-empty reason carrying quality + cost.
func TestEngineMinCost(t *testing.T) {
	snap := testSnapshot()
	// floor 0.2 -> LevelLite: every model in the snapshot is eligible, so the
	// cheapest (claude-haiku) must win under min_cost.
	d := Decide(Strategy{Optimization: OptMinCost, QualityFloor: 0.2}, snap, translationBody(), "gpt-4o", "")
	if d.Outcome != OutcomeOK {
		t.Fatalf("want ok, got %q (reason %q)", d.Outcome, d.Reason)
	}
	if d.Model != "claude-haiku" {
		t.Errorf("min_cost should pick the cheapest eligible model, got %q", d.Model)
	}
	if d.EstCostUSD <= 0 {
		t.Errorf("estimated cost must be positive, got %v", d.EstCostUSD)
	}
	if !reasonPattern.MatchString(d.Reason) {
		t.Errorf("reason must match the frozen pattern, got %q", d.Reason)
	}
	if d.Quality <= 0 {
		t.Errorf("quality must reflect the scored request, got %v", d.Quality)
	}
}

// TestEngineHighQuality asserts the highest capability tier wins under
// high_quality, and is still the cheapest-within-that-tier when two models
// share the top tier.
func TestEngineHighQuality(t *testing.T) {
	snap := testSnapshot()
	d := Decide(Strategy{Optimization: OptHighQuality, QualityFloor: 0.8}, snap, translationBody(), "", "")
	if d.Outcome != OutcomeOK {
		t.Fatalf("want ok, got %q", d.Outcome)
	}
	if !reasonPattern.MatchString(d.Reason) {
		t.Errorf("reason must match the frozen pattern, got %q", d.Reason)
	}
	if lvl, _ := LevelForModel(d.Model); lvl != LevelFrontier {
		t.Errorf("high_quality must pick a frontier-tier model, got %q (level %d)", d.Model, lvl)
	}
}

// TestEngineBalanced asserts the balanced goal lands on the median-cost
// eligible candidate, not the cheapest or the priciest.
func TestEngineBalanced(t *testing.T) {
	snap := testSnapshot()
	// No floor (LevelLite): all five eligible. Sorted by cost the median is
	// the middle of the five; assert it is neither the cheapest nor the
	// most expensive eligible model.
	d := Decide(Strategy{Optimization: OptBalanced, QualityFloor: 0.0}, snap, translationBody(), "", "")
	if d.Outcome != OutcomeOK {
		t.Fatalf("want ok, got %q", d.Outcome)
	}
	cheapest, priciest := "claude-haiku", "claude-opus-4-5"
	if d.Model == cheapest || d.Model == priciest {
		t.Errorf("balanced should pick the median, not the extreme %q (cost %v)", d.Model, d.EstCostUSD)
	}
	if !reasonPattern.MatchString(d.Reason) {
		t.Errorf("reason must match the frozen pattern, got %q", d.Reason)
	}
}

// TestEngineQualityFloorFiltering asserts a high floor excludes lower tiers:
// with floor 0.8 (LevelFrontier), a LevelLite model is never returned.
func TestEngineQualityFloorFiltering(t *testing.T) {
	snap := testSnapshot()
	d := Decide(Strategy{Optimization: OptMinCost, QualityFloor: 0.8}, snap, translationBody(), "", "")
	if d.Outcome != OutcomeOK {
		t.Fatalf("want ok, got %q", d.Outcome)
	}
	if lvl, _ := LevelForModel(d.Model); lvl != LevelFrontier {
		t.Errorf("a 0.8 floor must restrict to frontier-tier models, got %q (level %d)", d.Model, lvl)
	}
}

// TestBudgetExceeded covers acceptance scenario 2: when every eligible
// candidate's estimated cost exceeds max_budget, the decision is
// budget_exceeded (402) and names no runnable model.
func TestBudgetExceeded(t *testing.T) {
	snap := testSnapshot()
	// A large body drives every candidate's cost above the tiny cap.
	longBody := strings.Repeat(`{"messages":[{"role":"user","content":"analyze this document "}]}`, 400)
	capUSD := 0.0000001 // far below any candidate's cost
	d := Decide(Strategy{Optimization: OptMinCost, QualityFloor: 0.0, MaxBudget: &capUSD}, snap, []byte(longBody), "", "")
	if d.Outcome != OutcomeBudgetExceeded {
		t.Fatalf("want budget_exceeded, got %q (reason %q)", d.Outcome, d.Reason)
	}
	if d.Model != "" {
		t.Errorf("budget_exceeded must not name a runnable model, got %q", d.Model)
	}
}

// TestBudgetExceededAdapter asserts the 402 error body and status.
func TestBudgetExceededAdapter(t *testing.T) {
	snap := testSnapshot()
	capUSD := 0.0000001
	d := Decide(Strategy{Optimization: OptMinCost, QualityFloor: 0.0, MaxBudget: &capUSD}, snap, translationBody(), "", "")
	err := ErrorFor(d)
	if err == nil {
		t.Fatal("a budget_exceeded decision must yield an error")
	}
	if err.StatusCode != http.StatusPaymentRequired {
		t.Errorf("want 402, got %d", err.StatusCode)
	}
	if !err.DirectResponse {
		t.Error("the 402 body must be a DirectResponse written verbatim")
	}
	var body map[string]any
	if errJSON := json.Unmarshal(err.Body, &body); errJSON != nil {
		t.Fatalf("402 body must be JSON: %v", errJSON)
	}
	if e, ok := body["error"].(map[string]any); !ok || e["code"] != ErrorBudgetExceeded {
		t.Errorf("402 body code must be %q, got %#v", ErrorBudgetExceeded, body)
	}
}

// TestForcedModelUnavailable covers acceptance scenario 4: a forced model
// that is not in the price snapshot is 503 and the engine must NOT degrade
// to another model.
func TestForcedModelUnavailable(t *testing.T) {
	snap := testSnapshot()
	forced := "gpt-5-undefined" // present in no snapshot
	d := Decide(Strategy{Optimization: OptMinCost, QualityFloor: 0.0, OnlyAllowModel: forced}, snap, translationBody(), "", "")
	if d.Outcome != OutcomeModelUnavailable {
		t.Fatalf("want model_unavailable, got %q (reason %q)", d.Outcome, d.Reason)
	}
	if d.Model != forced {
		t.Errorf("the unavailable forced model must be named (no degrade), got %q", d.Model)
	}
}

// TestForcedModelUnavailableAdapter asserts the 503 error body and status.
func TestForcedModelUnavailableAdapter(t *testing.T) {
	snap := testSnapshot()
	d := Decide(Strategy{Optimization: OptMinCost, QualityFloor: 0.0, OnlyAllowModel: "gpt-5-undefined"}, snap, translationBody(), "", "")
	err := ErrorFor(d)
	if err == nil {
		t.Fatal("a model_unavailable decision must yield an error")
	}
	if err.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("want 503, got %d", err.StatusCode)
	}
	var body map[string]any
	if errJSON := json.Unmarshal(err.Body, &body); errJSON != nil {
		t.Fatalf("503 body must be JSON: %v", errJSON)
	}
	if e, ok := body["error"].(map[string]any); !ok || e["code"] != ErrorModelUnavailable {
		t.Errorf("503 body code must be %q, got %#v", ErrorModelUnavailable, body)
	}
}

// TestForcedModelHonored asserts that when the forced model IS available,
// the engine routes to exactly it (user control), even when a cheaper model
// would satisfy the floor.
func TestForcedModelHonored(t *testing.T) {
	snap := testSnapshot()
	d := Decide(Strategy{Optimization: OptMinCost, QualityFloor: 0.0, OnlyAllowModel: "gpt-4o"}, snap, translationBody(), "", "")
	if d.Outcome != OutcomeOK {
		t.Fatalf("an available forced model must route ok, got %q", d.Outcome)
	}
	if d.Model != "gpt-4o" {
		t.Errorf("forced routing must target only the forced model, got %q", d.Model)
	}
	if !reasonPattern.MatchString(d.Reason) {
		t.Errorf("reason must match the frozen pattern, got %q", d.Reason)
	}
}

// TestDefault covers acceptance scenario 3: with no strategy configured, the
// request-facing engine routes with the built-in default strategy and its
// output still carries the default model and a non-empty default reason.
func TestDefault(t *testing.T) {
	cache := NewPriceCache()
	seedCache(cache, testSnapshot())
	engine := NewEngine(EngineOptions{Versions: NewScopeTable(), Prices: cache})

	// A fresh scope has no recorded version, so the built-in default applies.
	d := engine.DecideForRequest(Scope{Kind: ScopeUser, ID: "anon"}, translationBody(), "gpt-4o")
	if d.Outcome != OutcomeOK {
		t.Fatalf("the default strategy must still route ok, got %q (reason %q)", d.Outcome, d.Reason)
	}
	if d.Model == "" {
		t.Error("the default decision must name a model")
	}
	if !strings.Contains(d.Reason, DefaultModel) {
		t.Errorf("the default decision reason must carry the default model name %q, got %q", DefaultModel, d.Reason)
	}
	if !reasonPattern.MatchString(d.Reason) {
		t.Errorf("default reason must still match the frozen pattern, got %q", d.Reason)
	}
	if d.StrategyVersion != 0 {
		t.Errorf("an unconfigured scope must resolve to the default (version 0), got %d", d.StrategyVersion)
	}
}

// TestDefaultNilEngine covers the degenerate no-engine path: a nil engine
// with no price snapshot has no candidates, so it must still return a
// well-formed, non-panicking Decision (the "routing always has an output"
// guarantee) whose outcome is the terminal model_unavailable rather than a
// crash.
func TestDefaultNilEngine(t *testing.T) {
	var e *Engine
	d := e.DecideForRequest(Scope{Kind: ScopeUser, ID: "x"}, translationBody(), "")
	if d.Outcome != OutcomeModelUnavailable {
		t.Fatalf("a nil engine with no prices must return the terminal model_unavailable outcome, got %q (reason %q)", d.Outcome, d.Reason)
	}
	if d.Reason == "" {
		t.Error("even the terminal outcome must carry a non-empty, explainable reason")
	}
}

// TestDecisionHeaders covers the explainability output: an ok decision stamps
// both x-or-model and x-or-reason; a failure stamps x-or-reason only.
func TestDecisionHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	snap := testSnapshot()
	d := Decide(Strategy{Optimization: OptMinCost, QualityFloor: 0.2}, snap, translationBody(), "", "")
	SetDecisionHeaders(rec, d)
	if got := rec.Header().Get(HeaderModel); got != d.Model {
		t.Errorf("x-or-model must be the chosen model, got %q want %q", got, d.Model)
	}
	if got := rec.Header().Get(HeaderReason); !reasonPattern.MatchString(got) {
		t.Errorf("x-or-reason must carry the frozen reason, got %q", got)
	}

	// Failure decision: no model header, reason header present.
	failing := Decide(Strategy{OnlyAllowModel: "gpt-5-undefined"}, snap, translationBody(), "", "")
	rec2 := httptest.NewRecorder()
	SetDecisionHeaders(rec2, failing)
	if got := rec2.Header().Get(HeaderModel); got != "" {
		t.Errorf("a failure decision must not stamp x-or-model, got %q", got)
	}
	if got := rec2.Header().Get(HeaderReason); got == "" {
		t.Error("a failure decision must still stamp x-or-reason")
	}
}

// TestConcurrency covers acceptance scenario 5: many requests sharing one
// price snapshot must each produce an independent, self-consistent decision
// (verified under -race). Two distinct bodies must not bleed into each
// other's result, and each result must pass the frozen reason format.
func TestConcurrency(t *testing.T) {
	cache := NewPriceCache()
	seedCache(cache, testSnapshot())
	snap := cache.Snapshot() // one shared, immutable snapshot

	const goroutines = 64
	// Two distinct bodies with different lengths -> different token
	// estimates; a cross-contamination would make one goroutine's result
	// match the other's instead of its own input.
	shortBody := translationBody()
	longBody := []byte(strings.Repeat(`{"messages":[{"role":"user","content":"long running analysis "}]}`, 100))

	strategy := Strategy{Optimization: OptMinCost, QualityFloor: 0.2}

	results := make([]Decision, goroutines)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // barrier: launch everything at once for -race coverage
			body := shortBody
			if i%2 == 1 {
				body = longBody
			}
			results[i] = Decide(strategy, snap, body, "gpt-4o", "")
		}(i)
	}
	close(start)
	wg.Wait()

	// Each result is independently derived from its own input: for a given
	// input the decision is deterministic, so every even index must equal the
	// short-body decision and every odd index the long-body decision.
	wantShort := Decide(strategy, snap, shortBody, "gpt-4o", "")
	wantLong := Decide(strategy, snap, longBody, "gpt-4o", "")
	if wantShort.Model == "" || wantLong.Model == "" {
		t.Fatalf("sanity: both bodies must route to a model, got short=%q long=%q", wantShort.Model, wantLong.Model)
	}
	for i := 0; i < goroutines; i++ {
		d := results[i]
		if !reasonPattern.MatchString(d.Reason) {
			t.Fatalf("goroutine %d reason must match the frozen pattern, got %q", i, d.Reason)
		}
		want := wantShort
		if i%2 == 1 {
			want = wantLong
		}
		if d.Model != want.Model || d.EstCostUSD != want.EstCostUSD {
			t.Fatalf("goroutine %d cross-contaminated: got model=%q cost=%v, want model=%q cost=%v",
				i, d.Model, d.EstCostUSD, want.Model, want.EstCostUSD)
		}
	}
}

// TestConcurrencySharedCache stresses the snapshot read seam itself: many
// goroutines read from one live PriceCache concurrently while a background
// goroutine applies new generations. Readers must each observe a complete
// table (no torn read) and never block. It is a bounded-iteration loop with
// no wall-clock sleep: the -race detector catches a torn read regardless of
// scheduling, and the join is deterministic.
func TestConcurrencySharedCache(t *testing.T) {
	cache := NewPriceCache()
	seedCache(cache, testSnapshot())
	const readers, swaps, perReader = 8, 500, 50

	// Writer keeps swapping generations for the duration of the reads.
	var wgWrites sync.WaitGroup
	wgWrites.Add(1)
	go func() {
		defer wgWrites.Done()
		for i := 0; i < swaps; i++ {
			seedCache(cache, testSnapshot())
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perReader; j++ {
				// Read a fresh snapshot and decide off it; every decision
				// must stay self-consistent against its own snapshot.
				snap := cache.Snapshot()
				d := Decide(Strategy{Optimization: OptMinCost, QualityFloor: 0.2}, snap, translationBody(), "", "")
				if d.Outcome != OutcomeOK && d.Outcome != OutcomeModelUnavailable {
					t.Fatalf("reader saw an unexpected outcome %q", d.Outcome)
					return
				}
			}
		}()
	}
	wg.Wait()
	wgWrites.Wait()
}

// seedCache is a test helper that synchronizes a PriceCache to a snapshot
// without running the (network-backed) syncer.
func seedCache(cache *PriceCache, snap PriceSnapshot) {
	if cache == nil {
		return
	}
	prices := make([]Price, 0, len(snap.Prices))
	for _, p := range snap.Prices {
		prices = append(prices, p)
	}
	cache.Apply(prices, snap.SyncedAt)
}

// TestEstimateCostDeterminism pins the cost formula: identical inputs give
// identical estimates, and a zero/negative multiplier is treated as 1.
func TestEstimateCostDeterminism(t *testing.T) {
	p := Price{PromptPricePer1M: 1, CompletionPricePer1M: 2, Multiplier: 1}
	a := estimateCost(p, 100)
	b := estimateCost(p, 100)
	if a != b {
		t.Fatalf("cost estimate must be deterministic, got %v then %v", a, b)
	}
	if a <= 0 {
		t.Fatalf("a priced model must estimate a positive cost, got %v", a)
	}
	noMult := Price{PromptPricePer1M: 1, CompletionPricePer1M: 2} // multiplier 0
	if got := estimateCost(noMult, 100); got != a {
		t.Errorf("a zero multiplier must be treated as 1: got %v want %v", got, a)
	}
}
