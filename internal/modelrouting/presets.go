package modelrouting

// The three built-in strategy presets (RIC-2525 / OPENROUT-002-S3): cheap,
// balanced, and high_quality. They are the starting points for the offline
// comparison: each is a plain Strategy that users may copy into their own
// configuration (the ticket's "用户可复制为自定义策略"), so they carry no
// forced-model pin or budget cap — the comparison isolates the
// optimization goal and the quality floor as the only moving parts.
//
// The floor values sit on the S1-B quality-band edges (quality.go): 0.35 is
// the top of the LevelLite band, 0.55 the top of the LevelStandard band.
// "cheap" therefore routes to the cheapest tier that any reasonable request
// needs, while "high_quality" routes within the top two tiers — under one
// shared price snapshot the cheap preset's total cost is always
// ≤ high_quality's, which the comparison report asserts field-wise rather
// than in absolute dollars.

// Preset names are the frozen report row labels.
const (
	// PresetCheap is the cost-first preset.
	PresetCheap = "cheap"
	// PresetBalanced is the midpoint preset.
	PresetBalanced = "balanced"
	// PresetHighQuality is the quality-first preset.
	PresetHighQuality = "high_quality"
)

// Preset describes one built-in strategy template.
type Preset struct {
	// Name is the frozen preset label (report row key).
	Name string
	// Description is the operator-facing one-liner.
	Description string
	// Strategy is the copy-ready strategy the preset runs with.
	Strategy Strategy
}

// PresetStrategies are the three built-in templates in cheap →
// high_quality order. They are validated at init (they must always load
// cleanly) and are safe for concurrent reads.
var PresetStrategies = []Preset{
	{
		Name:        PresetCheap,
		Description: "cost-first routing: lowest floor, cheapest eligible model",
		Strategy: Strategy{
			Optimization: OptMinCost,
			QualityFloor: 0.35,
		},
	},
	{
		Name:        PresetBalanced,
		Description: "midpoint routing: standard-tier floor, cost-median model",
		Strategy: Strategy{
			Optimization: OptBalanced,
			QualityFloor: 0.55,
		},
	},
	{
		Name:        PresetHighQuality,
		Description: "quality-first routing: advanced-tier floor, strongest eligible model",
		Strategy: Strategy{
			Optimization: OptHighQuality,
			QualityFloor: 0.75,
		},
	},
}

func init() {
	for _, p := range PresetStrategies {
		if err := Validate(p.Strategy); err != nil {
			// A preset that fails validation would poison every replay
			// run; failing at init turns that into a boot-time error.
			panic("modelrouting: preset " + p.Name + " is invalid: " + err.Error())
		}
	}
}

// PresetStrategy resolves one preset by name. ok=false means the name is
// not one of the three built-ins (user presets are Strategies, not
// Presets, so an unknown name is a caller bug, not a fallback case).
func PresetStrategy(name string) (Preset, bool) {
	for _, p := range PresetStrategies {
		if p.Name == name {
			return p, true
		}
	}
	return Preset{}, false
}
