package modelrouting

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestStrategyValidation covers acceptance scenario 1: a legal strategy
// document loads and every field passes type/range checks, while invalid
// inputs are rejected with the offending field named — never silently
// coerced.
func TestStrategyValidation(t *testing.T) {
	legal := `{"optimization":"min_cost","quality_floor":0.7,"max_budget":0.005}`
	s, err := StrategyFromJSON([]byte(legal))
	if err != nil {
		t.Fatalf("legal strategy rejected: %v", err)
	}
	if s.Optimization != OptMinCost || s.QualityFloor != 0.7 || s.MaxBudget == nil || *s.MaxBudget != 0.005 {
		t.Fatalf("unexpected decoded strategy: %+v", s)
	}

	cases := []struct {
		name    string
		doc     string
		wantSub string
	}{
		{"quality floor above 1", `{"optimization":"balanced","quality_floor":1.5}`, "quality_floor"},
		{"quality floor below 0", `{"optimization":"balanced","quality_floor":-0.1}`, "quality_floor"},
		{"negative budget", `{"optimization":"min_cost","quality_floor":0.5,"max_budget":-1}`, "max_budget"},
		{"zero budget", `{"optimization":"min_cost","quality_floor":0.5,"max_budget":0}`, "max_budget"},
		{"unknown optimization", `{"optimization":"fast","quality_floor":0.5}`, "optimization"},
		{"missing optimization", `{"quality_floor":0.5}`, "optimization"},
		{"empty document", ``, "empty"},
		{"malformed JSON", `{"optimization":"min_cost"`, "invalid JSON"},
		// A type violation (a string where a number belongs) must be a
		// field-named rejection, not a panic or a silent zero.
		{"typed quality floor", `{"optimization":"min_cost","quality_floor":"0.7"}`, "quality_floor"},
	}
	for _, tc := range cases {
		if _, err := StrategyFromJSON([]byte(tc.doc)); err == nil {
			t.Errorf("%s: expected rejection, got success", tc.name)
			continue
		} else if !strings.Contains(err.Error(), tc.wantSub) {
			t.Errorf("%s: error %q does not name the offending field", tc.name, err)
		}
	}
}

// TestStrategyOptions covers the optional-field shapes: preferred_model and
// only_allow_model are optional, and a strategy with no budget is legal.
func TestStrategyOptions(t *testing.T) {
	s, err := StrategyFromJSON([]byte(`{"optimization":"high_quality","quality_floor":0.9,"preferred_model":"claude-opus-4-7","only_allow_model":"gpt-4o"}`))
	if err != nil {
		t.Fatalf("legal optional-field strategy rejected: %v", err)
	}
	if s.PreferredModel != "claude-opus-4-7" || s.OnlyAllowModel != "gpt-4o" {
		t.Fatalf("optional fields not decoded: %+v", s)
	}

	// max_budget absent is valid and must decode as a nil pointer.
	s, err = StrategyFromJSON([]byte(`{"optimization":"min_cost","quality_floor":0.2}`))
	if err != nil {
		t.Fatalf("no-budget strategy rejected: %v", err)
	}
	if s.MaxBudget != nil {
		t.Fatalf("absent max_budget must decode as nil, got %v", *s.MaxBudget)
	}

	// The JSON contract must be stable for the Stage 2 consumer: encode a
	// full strategy and check the frozen field names round-trip.
	full := Strategy{Optimization: OptBalanced, QualityFloor: 0.5, PreferredModel: "m", OnlyAllowModel: "m"}
	budget := 0.001
	full.MaxBudget = &budget
	enc, err := json.Marshal(full)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	round, err := StrategyFromJSON(enc)
	if err != nil {
		t.Fatalf("round-trip rejected: %v", err)
	}
	if round.Optimization != full.Optimization || round.QualityFloor != full.QualityFloor ||
		round.PreferredModel != full.PreferredModel || round.OnlyAllowModel != full.OnlyAllowModel {
		t.Fatalf("round-trip mismatch: %+v vs %+v", round, full)
	}
	// The optional pointer must round-trip its value, not its address.
	if round.MaxBudget == nil || *round.MaxBudget != *full.MaxBudget {
		t.Fatalf("max_budget did not round-trip: %v vs %v", round.MaxBudget, full.MaxBudget)
	}
}
