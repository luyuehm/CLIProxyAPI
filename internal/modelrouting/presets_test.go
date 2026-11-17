package modelrouting

import (
	"testing"
)

// The preset pinning tests (RIC-2525 contract VERIFY: TestPresets*). The
// three templates are the comparison's fixed points, so their values are
// pinned literally against the S1-B frozen quality-band edges rather than
// self-compared against the live table (a self-compare would let a table
// edit pass its own test).

// TestPresetsExist pins the three frozen preset names in the contract's
// order: cheap, balanced, high_quality.
func TestPresetsExist(t *testing.T) {
	if len(PresetStrategies) != 3 {
		t.Fatalf("exactly 3 built-in presets must ship, got %d", len(PresetStrategies))
	}
	wantNames := []string{PresetCheap, PresetBalanced, PresetHighQuality}
	for i, want := range wantNames {
		if PresetStrategies[i].Name != want {
			t.Errorf("preset %d must be %q in the frozen order, got %q", i, want, PresetStrategies[i].Name)
		}
	}
	if _, ok := PresetStrategy("nonexistent"); ok {
		t.Error("an unknown preset name must not resolve")
	}
}

// TestPresetsValues pins each preset's strategy against literal values on
// the frozen quality-band edges: cheap = min_cost at the LevelLite band
// top (0.35), balanced = balanced at the LevelStandard band top (0.55),
// high_quality = high_quality at the LevelAdvanced band top (0.75). No
// preset carries a forced-model pin or a budget cap: the comparison
// isolates the optimization goal and floor as its only moving parts.
func TestPresetsValues(t *testing.T) {
	want := map[string]Strategy{
		PresetCheap:       {Optimization: OptMinCost, QualityFloor: 0.35},
		PresetBalanced:    {Optimization: OptBalanced, QualityFloor: 0.55},
		PresetHighQuality: {Optimization: OptHighQuality, QualityFloor: 0.75},
	}
	for _, p := range PresetStrategies {
		s, ok := want[p.Name]
		if !ok {
			t.Errorf("preset %q is not one of the three frozen templates", p.Name)
			continue
		}
		if p.Strategy != s {
			t.Errorf("preset %q must be exactly %+v, got %+v", p.Name, s, p.Strategy)
		}
		if p.Strategy.OnlyAllowModel != "" || p.Strategy.MaxBudget != nil || p.Strategy.PreferredModel != "" {
			t.Errorf("presets carry no forced model / budget cap / preferred tie-break, got %+v", p.Strategy)
		}
		if p.Description == "" {
			t.Errorf("preset %q must carry a non-empty description", p.Name)
		}
	}
}

// TestPresetsBandEdges pins the floors to the S1-B band edges literally, so
// a shift of the quality bands in quality.go that silently reclassifies a
// preset floor is caught: cheap must be the top of the LevelLite band,
// balanced the top of LevelStandard, high_quality the top of LevelAdvanced.
func TestPresetsBandEdges(t *testing.T) {
	edges := []struct {
		name  string
		edge  float64
		level CapabilityLevel
	}{
		{PresetCheap, 0.35, LevelLite},
		{PresetBalanced, 0.55, LevelStandard},
		{PresetHighQuality, 0.75, LevelAdvanced},
	}
	for _, e := range edges {
		p, ok := PresetStrategy(e.name)
		if !ok {
			t.Fatalf("preset %q must resolve", e.name)
		}
		if p.Strategy.QualityFloor != e.edge {
			t.Errorf("%s floor must sit on the %v band edge %v, got %v",
				e.name, e.level, e.edge, p.Strategy.QualityFloor)
		}
		if got := levelForFloor(p.Strategy.QualityFloor); got != e.level {
			t.Errorf("%s floor %v must map to the %v tier, got tier %v",
				e.name, p.Strategy.QualityFloor, e.level, got)
		}
	}
}

// TestPresetsCopyable pins the "user copies a preset into a custom
// strategy" path: a copied preset passes the S1-A validation the ScopeTable
// would apply, and a mutated copy (e.g. a tightened floor) validates
// independently of the original.
func TestPresetsCopyable(t *testing.T) {
	for _, p := range PresetStrategies {
		copied := p.Strategy
		copied.QualityFloor = 0.9
		if err := Validate(copied); err != nil {
			t.Errorf("a copied preset with a tightened floor must validate, got %v", err)
		}
		// The table entry is untouched by the copy (no aliasing).
		if p.Strategy.QualityFloor == 0.9 {
			t.Errorf("copying a preset must not mutate the table entry %q", p.Name)
		}
	}
}
