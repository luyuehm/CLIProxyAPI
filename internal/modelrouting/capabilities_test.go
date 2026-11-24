package modelrouting

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
)

func bodyWithRequiredCapabilities(raw string) []byte {
	// Keep the same message content as translationBody() so a request with an
	// empty constraint scores identically to the unconstrained baseline.
	return []byte(`{"messages":[{"role":"user","content":"Please translate the following sentence into French: good morning, how are you today?"}],"required_capabilities":` + raw + `}`)
}

// TestRequiredCapabilitiesFiltering covers acceptance scenario 1: a request
// that requires structured_outputs must never select a model that does not
// advertise that capability; the final model comes from the supporting set
// and the non-supporting candidate is reported as blocked.
func TestRequiredCapabilitiesFiltering(t *testing.T) {
	snap := testSnapshot()
	d := Decide(
		Strategy{Optimization: OptMinCost, QualityFloor: 0.0},
		snap,
		bodyWithRequiredCapabilities(`["structured_outputs"]`),
		"gpt-4o",
		"",
	)
	if d.Outcome != OutcomeOK {
		t.Fatalf("want ok, got %q (reason %q)", d.Outcome, d.Reason)
	}
	// claude-haiku is the cheapest model but is a Lite-tier model that the
	// conservative capability table does not credit with structured_outputs.
	// It must be excluded even though it wins the unconstrained min_cost.
	if d.Model == "claude-haiku" {
		t.Fatalf("structured_outputs constraint must exclude claude-haiku, got %q", d.Model)
	}
	if missing := missingCapabilities(d.Model, []string{CapabilityStructuredOutputs}); len(missing) > 0 {
		t.Errorf("chosen model %q must satisfy the constraint, missing %v", d.Model, missing)
	}
	foundBlocked := false
	for _, blocked := range d.BlockedModels {
		if blocked.Model == "claude-haiku" {
			foundBlocked = true
			if !containsString(blocked.MissingCapabilities, CapabilityStructuredOutputs) {
				t.Errorf("claude-haiku must report structured_outputs as missing, got %v", blocked.MissingCapabilities)
			}
		}
	}
	if !foundBlocked {
		t.Errorf("expected claude-haiku in blocked_models, got %v", d.BlockedModels)
	}
}

// TestRequiredCapabilitiesAllBlocked covers acceptance scenario 2: when every
// candidate lacks a required capability, the decision is a 400
// capability_constraint_violation carrying the full blocked list with each
// model's missing capability.
func TestRequiredCapabilitiesAllBlocked(t *testing.T) {
	snap := testSnapshot()
	d := Decide(
		Strategy{Optimization: OptMinCost, QualityFloor: 0.0},
		snap,
		bodyWithRequiredCapabilities(`["xyz_capability"]`),
		"",
		"",
	)
	if d.Outcome != OutcomeConstraintViolation {
		t.Fatalf("want constraint_violation, got %q", d.Outcome)
	}
	if len(d.BlockedModels) != len(snap.Prices) {
		t.Fatalf("expected every candidate blocked, got %d of %d", len(d.BlockedModels), len(snap.Prices))
	}
	for _, blocked := range d.BlockedModels {
		if !containsString(blocked.MissingCapabilities, "xyz_capability") {
			t.Errorf("blocked model %q must report xyz_capability missing, got %v", blocked.Model, blocked.MissingCapabilities)
		}
	}

	errMsg := ErrorFor(d)
	if errMsg == nil {
		t.Fatal("constraint_violation must map to an error message")
	}
	if errMsg.StatusCode != http.StatusBadRequest {
		t.Errorf("want 400, got %d", errMsg.StatusCode)
	}
	var body map[string]any
	if err := json.Unmarshal(errMsg.Body, &body); err != nil {
		t.Fatalf("error body must be valid JSON: %v", err)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj == nil || errObj["code"] != ErrorCapabilityConstraint {
		t.Errorf("error body must carry code %q, got %v", ErrorCapabilityConstraint, errObj)
	}
	blockedModels, ok := errObj["blocked_models"].([]any)
	if !ok || len(blockedModels) != len(snap.Prices) {
		t.Errorf("error body must carry the full blocked_models list, got %v", errObj["blocked_models"])
	}
}

// TestRequiredCapabilitiesFallbackRespectsConstraint covers acceptance
// scenario 3: the kept candidate set — the only set any fallback may draw
// from — contains only models that satisfy the constraint, and every blocked
// model is one that does not.
func TestRequiredCapabilitiesFallbackRespectsConstraint(t *testing.T) {
	snap := testSnapshot()
	required := []string{CapabilityStructuredOutputs, CapabilityFunctionCalling}
	kept, blocked := filterByCapabilities(allTestCandidates(snap), required)
	if len(kept) == 0 {
		t.Fatal("expected at least one candidate to satisfy the constraint")
	}
	for _, c := range kept {
		if missing := missingCapabilities(c.name, required); len(missing) > 0 {
			t.Errorf("fallback candidate %q must satisfy the constraint, missing %v", c.name, missing)
		}
	}
	for _, b := range blocked {
		if missing := missingCapabilities(b.Model, required); len(missing) == 0 {
			t.Errorf("blocked model %q must actually lack a required capability", b.Model)
		}
	}
}

// TestRequiredCapabilitiesEmptyBehavesIdentically covers acceptance scenario
// 4: a request without required_capabilities routes exactly as before — no
// filtering, no blocked models.
func TestRequiredCapabilitiesEmptyBehavesIdentically(t *testing.T) {
	snap := testSnapshot()
	baseline := Decide(Strategy{Optimization: OptBalanced, QualityFloor: 0.0}, snap, translationBody(), "", "")
	withEmpty := Decide(Strategy{Optimization: OptBalanced, QualityFloor: 0.0}, snap, bodyWithRequiredCapabilities(`[]`), "", "")
	// The added required_capabilities field makes the body a few bytes longer,
	// so the cost estimate (and the reason's cost segment) legitimately moves
	// by a rounding step. What must be identical is the routing behavior: the
	// same outcome, the same chosen model, and no capability filtering.
	if baseline.Outcome != withEmpty.Outcome || baseline.Model != withEmpty.Model {
		t.Fatalf("empty constraint must not change routing: baseline=%q/%q withEmpty=%q/%q",
			baseline.Outcome, baseline.Model, withEmpty.Outcome, withEmpty.Model)
	}
	if len(withEmpty.BlockedModels) != 0 {
		t.Errorf("empty constraint must not filter any candidate, got %v", withEmpty.BlockedModels)
	}
}

// TestRequiredCapabilitiesForcedModelConstraint ensures the hard constraint
// outranks a forced-model pin: a forced model that lacks the capability is a
// 400, never a silent degrade to another model.
func TestRequiredCapabilitiesForcedModelConstraint(t *testing.T) {
	snap := testSnapshot()
	d := Decide(
		Strategy{Optimization: OptMinCost, QualityFloor: 0.0, OnlyAllowModel: "claude-haiku"},
		snap,
		bodyWithRequiredCapabilities(`["structured_outputs"]`),
		"claude-haiku",
		"",
	)
	if d.Outcome != OutcomeConstraintViolation {
		t.Fatalf("want constraint_violation for a forced model that lacks the capability, got %q", d.Outcome)
	}
	if d.Model != "claude-haiku" {
		t.Errorf("the blocked forced model must be named, got %q", d.Model)
	}
	if len(d.BlockedModels) != 1 || d.BlockedModels[0].Model != "claude-haiku" {
		t.Errorf("forced-model constraint must name claude-haiku as blocked, got %v", d.BlockedModels)
	}
}

// TestRequiredCapabilitiesNoPricedCandidates ensures a capability constraint
// does not mask the existing "no eligible candidate" semantics: with no
// priced candidates at all the outcome stays model_unavailable (503), not a
// spurious 400 with an empty blocked list.
func TestRequiredCapabilitiesNoPricedCandidates(t *testing.T) {
	d := Decide(
		Strategy{Optimization: OptMinCost, QualityFloor: 0.0},
		PriceSnapshot{},
		bodyWithRequiredCapabilities(`["structured_outputs"]`),
		"",
		"",
	)
	if d.Outcome != OutcomeModelUnavailable {
		t.Fatalf("want model_unavailable when no candidates are priced, got %q", d.Outcome)
	}
	if len(d.BlockedModels) != 0 {
		t.Errorf("no candidates priced means nothing was blocked by capability, got %v", d.BlockedModels)
	}
}

// TestRequiredCapabilitiesParsing covers the request-level parameter forms:
// JSON array, comma-separated string, case/whitespace normalization, and the
// absent/empty no-op.
func TestRequiredCapabilitiesParsing(t *testing.T) {
	cases := []struct {
		name string
		body []byte
		want []string
	}{
		{"array", []byte(`{"required_capabilities":["structured_outputs","function_calling"]}`), []string{CapabilityFunctionCalling, CapabilityStructuredOutputs}},
		{"string", []byte(`{"required_capabilities":"structured_outputs,function_calling"}`), []string{CapabilityFunctionCalling, CapabilityStructuredOutputs}},
		{"single string", []byte(`{"required_capabilities":"structured_outputs"}`), []string{CapabilityStructuredOutputs}},
		{"case and spaces", []byte(`{"required_capabilities":[" Structured_Outputs ", "FUNCTION_CALLING", "structured_outputs"]}`), []string{CapabilityFunctionCalling, CapabilityStructuredOutputs}},
		{"empty array", []byte(`{"required_capabilities":[]}`), nil},
		{"absent", []byte(`{"model":"claude-sonnet"}`), nil},
		{"empty string", []byte(`{"required_capabilities":""}`), nil},
		{"null", []byte(`{"required_capabilities":null}`), nil},
		{"blank body", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := requiredCapabilitiesFromBody(tc.body)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("requiredCapabilitiesFromBody(%s) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

// allTestCandidates mirrors the priced candidate universe built inside Decide
// so the fallback-set property can be asserted directly.
func allTestCandidates(snap PriceSnapshot) []candidate {
	cands := make([]candidate, 0, len(snap.Prices))
	for name, p := range snap.Prices {
		cands = append(cands, candidate{name: name, level: levelFor(name), estCost: estimateCost(p, 100)})
	}
	return cands
}
