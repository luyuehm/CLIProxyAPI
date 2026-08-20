// Package costallocation tracks provider request spend per department, team,
// or project by allocating each usage record to a department based on its API
// key or authentication identity. It plugs into the existing usage accounting
// pipeline as a usage.Plugin, exactly like the redisqueue usage plugin.
package costallocation

// Period describes the time window over which spend is aggregated.
type Period string

const (
	// PeriodMonthly resets every calendar month.
	PeriodMonthly Period = "monthly"
	// PeriodQuarterly resets every calendar quarter (Jan/Apr/Jul/Oct).
	PeriodQuarterly Period = "quarterly"
)

// ParsePeriod converts a plain string to a Period. Unknown or empty values
// fall back to PeriodMonthly.
func ParsePeriod(s string) Period {
	switch Period(s) {
	case PeriodQuarterly:
		return PeriodQuarterly
	default:
		return PeriodMonthly
	}
}

// PriceRate prices a single token bucket in a given currency unit per 1000
// tokens. Empty Model applies the rate to any model that does not match a
// more specific entry.
type PriceRate struct {
	// Model is the model name this rate applies to. Empty matches all models
	// not matched by a more specific entry.
	Model string `yaml:"model,omitempty" json:"model,omitempty"`
	// Provider constrains the rate to a provider when set. Empty matches any.
	Provider string `yaml:"provider,omitempty" json:"provider,omitempty"`
	// InputRatePer1K prices uncached and cache-write input tokens.
	InputRatePer1K float64 `yaml:"input-rate-per-1k,omitempty" json:"input-rate-per-1k,omitempty"`
	// OutputRatePer1K prices output tokens (non-reasoning + reasoning).
	OutputRatePer1K float64 `yaml:"output-rate-per-1k,omitempty" json:"output-rate-per-1k,omitempty"`
	// CacheReadRatePer1K prices cache-read input tokens (usually cheaper).
	CacheReadRatePer1K float64 `yaml:"cache-read-rate-per-1k,omitempty" json:"cache-read-rate-per-1k,omitempty"`
	// CacheWriteRatePer1K prices cache-write (creation) input tokens.
	CacheWriteRatePer1K float64 `yaml:"cache-write-rate-per-1k,omitempty" json:"cache-write-rate-per-1k,omitempty"`
	// ReasoningRatePer1K, when > 0, overrides OutputRatePer1K for reasoning
	// tokens. Zero falls back to OutputRatePer1K for reasoning tokens.
	ReasoningRatePer1K float64 `yaml:"reasoning-rate-per-1k,omitempty" json:"reasoning-rate-per-1k,omitempty"`
}

// DepartmentRule describes how usage records are allocated to a department.
// A record matches a rule when any of its criteria match. The first matching
// rule in the configured rule list wins, so more specific rules should be
// listed first.
type DepartmentRule struct {
	// Name is the department/team/project the matching records are charged to.
	Name string `yaml:"name" json:"name"`
	// APIKeys matches when the record's client API key equals one of these
	// values. Empty means "does not match on API key".
	APIKeys []string `yaml:"api-keys,omitempty" json:"api-keys,omitempty"`
	// APIKeyPrefixes matches when the record's client API key starts with one
	// of these prefixes.
	APIKeyPrefixes []string `yaml:"api-key-prefixes,omitempty" json:"api-key-prefixes,omitempty"`
	// APIKeySuffixes matches when the record's client API key ends with one of
	// these suffixes.
	APIKeySuffixes []string `yaml:"api-key-suffixes,omitempty" json:"api-key-suffixes,omitempty"`
	// AuthIDs matches when the record's auth credential ID equals one of these
	// values. Auth files carry stable IDs, so this survives key rotation.
	AuthIDs []string `yaml:"auth-ids,omitempty" json:"auth-ids,omitempty"`
	// AuthIDPrefixes matches when the record's auth credential ID starts with
	// one of these prefixes.
	AuthIDPrefixes []string `yaml:"auth-id-prefixes,omitempty" json:"auth-id-prefixes,omitempty"`
	// Default, when true, is the catch-all rule: records that do not match any
	// other rule are allocated here. At most one rule may set Default.
	Default bool `yaml:"default,omitempty" json:"default,omitempty"`
}

// CostAllocationConfig is the user-facing configuration for department-level
// cost allocation.
type CostAllocationConfig struct {
	// Enabled toggles the cost allocation tracker. When false, usage records
	// are not aggregated and report endpoints return an empty state.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Currency is the free-form currency unit for reported spend
	// (e.g. "USD", "CNY"). Purely informational.
	Currency string `yaml:"currency,omitempty" json:"currency,omitempty"`
	// UnallocatedName names the implicit catch-all department for records that
	// match no rule. Defaults to "unallocated" when empty.
	UnallocatedName string `yaml:"unallocated-name,omitempty" json:"unallocated-name,omitempty"`
	// Rules is the ordered list of department allocation rules. The first rule
	// matching a record wins. When no rule matches and no rule has Default set,
	// the record is allocated to UnallocatedName.
	Rules []DepartmentRule `yaml:"rules,omitempty" json:"rules,omitempty"`
	// Period is the summary window used by report endpoints and exports.
	// One of "monthly" or "quarterly". Defaults to "monthly".
	Period Period `yaml:"period,omitempty" json:"period,omitempty"`
	// Prices is the ordered price table. The first matching entry for a given
	// (provider, model) pair wins; a later, more specific entry does NOT
	// override an earlier broader one, so list specific rates first.
	Prices []PriceRate `yaml:"prices,omitempty" json:"prices,omitempty"`
}

// effectiveUnallocatedName returns UnallocatedName with a sane default.
func (c CostAllocationConfig) effectiveUnallocatedName() string {
	if name := trimSpace(c.UnallocatedName); name != "" {
		return name
	}
	return "unallocated"
}

// effectivePeriod returns the summary window with a sane default.
func (c CostAllocationConfig) effectivePeriod() Period {
	if c.Period != "" {
		return c.Period
	}
	return PeriodMonthly
}

func trimSpace(s string) string {
	if s == "" {
		return ""
	}
	start, end := 0, len(s)
	for start < end && isSpaceByte(s[start]) {
		start++
	}
	for end > start && isSpaceByte(s[end-1]) {
		end--
	}
	return s[start:end]
}

func isSpaceByte(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\v', '\f', '\r', 0x85, 0xA0:
		return true
	default:
		return false
	}
}
