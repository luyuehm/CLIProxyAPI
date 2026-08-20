package costallocation

// PriceRate prices token buckets in a given currency unit per 1000 tokens.
// Empty Model and Provider act as wildcards.
type PriceRate struct {
	// Model is the model name this rate applies to. Empty matches all models.
	Model string `yaml:"model,omitempty" json:"model,omitempty"`
	// Provider constrains the rate to a provider when set. Empty matches any.
	Provider string `yaml:"provider,omitempty" json:"provider,omitempty"`
	// InputRatePer1K prices uncached input tokens per 1000 tokens.
	InputRatePer1K float64 `yaml:"input-rate-per-1k,omitempty" json:"input-rate-per-1k,omitempty"`
	// OutputRatePer1K prices output tokens (non-reasoning + reasoning) per 1000 tokens.
	OutputRatePer1K float64 `yaml:"output-rate-per-1k,omitempty" json:"output-rate-per-1k,omitempty"`
	// CacheReadRatePer1K prices cache-read input tokens per 1000 tokens.
	CacheReadRatePer1K float64 `yaml:"cache-read-rate-per-1k,omitempty" json:"cache-read-rate-per-1k,omitempty"`
	// CacheWriteRatePer1K prices cache-write (creation) input tokens per 1000 tokens.
	CacheWriteRatePer1K float64 `yaml:"cache-write-rate-per-1k,omitempty" json:"cache-write-rate-per-1k,omitempty"`
	// ReasoningRatePer1K, when > 0, overrides OutputRatePer1K for reasoning tokens.
	ReasoningRatePer1K float64 `yaml:"reasoning-rate-per-1k,omitempty" json:"reasoning-rate-per-1k,omitempty"`
}

// AllocationRule defines criteria to map a request to Department, Team, and Project.
type AllocationRule struct {
	// Department is the target department name (e.g. "Engineering", "Marketing").
	Department string `yaml:"department,omitempty" json:"department,omitempty"`
	// Team is the target team name (e.g. "Platform", "Search", "Data Science").
	Team string `yaml:"team,omitempty" json:"team,omitempty"`
	// Project is the target project name (e.g. "Chatbot", "Copilot", "Translation").
	Project string `yaml:"project,omitempty" json:"project,omitempty"`
	// APIKeys is a list of exact API keys that map to this rule.
	APIKeys []string `yaml:"api-keys,omitempty" json:"api-keys,omitempty"`
	// APIKeyPrefixes matches API keys starting with any of these prefixes.
	APIKeyPrefixes []string `yaml:"api-key-prefixes,omitempty" json:"api-key-prefixes,omitempty"`
	// Tags matches key-value pairs in request metadata or headers.
	Tags map[string]string `yaml:"tags,omitempty" json:"tags,omitempty"`
	// Headers matches exact request/response HTTP header values (case-insensitive key).
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
}

// AllocationConfig holds user-facing configuration for department cost allocation.
type AllocationConfig struct {
	// Enabled toggles the cost allocation plugin.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Currency is the currency unit for reporting (e.g. "USD", "CNY"). Default: "USD".
	Currency string `yaml:"currency,omitempty" json:"currency,omitempty"`
	// DefaultDepartment is the department assigned when no rule matches. Default: "unallocated".
	DefaultDepartment string `yaml:"default-department,omitempty" json:"default-department,omitempty"`
	// DefaultTeam is the team assigned when no rule matches.
	DefaultTeam string `yaml:"default-team,omitempty" json:"default-team,omitempty"`
	// DefaultProject is the project assigned when no rule matches.
	DefaultProject string `yaml:"default-project,omitempty" json:"default-project,omitempty"`
	// Rules is the ordered list of allocation rules. First matching rule wins.
	Rules []AllocationRule `yaml:"rules,omitempty" json:"rules,omitempty"`
	// Prices is the ordered price table for token-to-cost evaluation.
	Prices []PriceRate `yaml:"prices,omitempty" json:"prices,omitempty"`
	// TagKeys specifies custom header names to inspect for department/team/project tags.
	// E.g., ["X-Department", "X-Team", "X-Project", "X-Cost-Center"].
	TagKeys []string `yaml:"tag-keys,omitempty" json:"tag-keys,omitempty"`
}
