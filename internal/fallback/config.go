package fallback

import (
	"fmt"
	"strings"
	"time"
)

// Config is the declarative, YAML/JSON-expressible fallback strategy. It is
// the operator-facing shape; Policy is the execution shape the orchestrator
// consumes. Keeping the two separate means the gateway can evolve its
// execution model without breaking the on-disk strategy files, and the
// strategy stays serializable with no gateway imports.
//
// The issue's out-of-scope note deliberately rules out an external policy
// DSL: a typed config block like this one is all that is needed.
type Config struct {
	// Providers is the ordered candidate chain; the first entry is the
	// preferred provider.
	Providers []ProviderRef `yaml:"providers" json:"providers"`

	// CostLimitUSD is the hard per-request cost ceiling. 0 disables it.
	CostLimitUSD float64 `yaml:"cost-limit-usd,omitempty" json:"cost_limit_usd,omitempty"`
	// QualityFloor is the minimum quality index a candidate's known
	// estimate must reach. 0 disables it.
	QualityFloor float64 `yaml:"quality-floor,omitempty" json:"quality_floor,omitempty"`
	// TimeoutBudget caps total wall time across all attempts. Empty or
	// "0" disables it. Accepts Go duration strings ("30s", "1m").
	TimeoutBudget string `yaml:"timeout-budget,omitempty" json:"timeout_budget,omitempty"`

	// BackoffWindow is how far back the health tracker counts failures.
	// Defaults to 5m when empty.
	BackoffWindow string `yaml:"backoff-window,omitempty" json:"backoff_window,omitempty"`
	// BackoffBase is the first backoff cooldown. Defaults to 5s.
	BackoffBase string `yaml:"backoff-base,omitempty" json:"backoff_base,omitempty"`
	// BackoffMax caps the backoff cooldown. Defaults to 5m.
	BackoffMax string `yaml:"backoff-max,omitempty" json:"backoff_max,omitempty"`
}

// ProviderRef is one candidate slot in the chain.
type ProviderRef struct {
	// Provider is the provider key the gateway can execute.
	Provider string `yaml:"provider" json:"provider"`
	// Model is an optional per-candidate model override.
	Model string `yaml:"model,omitempty" json:"model,omitempty"`
	// Quality is an optional known quality estimate.
	Quality float64 `yaml:"quality,omitempty" json:"quality,omitempty"`
	// CostUSD is an optional known per-request cost estimate.
	CostUSD float64 `yaml:"cost-usd,omitempty" json:"cost_usd,omitempty"`
	// Private marks the provider as a private endpoint (served from the
	// customer's own cloud account or a dedicated deployment). When a
	// request sets require_private, only these candidates may be
	// attempted and no public provider is called.
	Private bool `yaml:"private,omitempty" json:"private,omitempty"`
}

// ToPolicy converts the declarative config into the execution policy. It
// reports a parse error when the timeout budget does not parse.
func (c Config) ToPolicy() (Policy, *HealthWindow, error) {
	p := Policy{
		QualityFloor: c.QualityFloor,
		CostLimit:    c.CostLimitUSD,
	}
	for _, ref := range c.Providers {
		p.Candidates = append(p.Candidates, Candidate{
			Provider: ref.Provider,
			Model:    ref.Model,
			Quality:  ref.Quality,
			CostUSD:  ref.CostUSD,
			Private:  ref.Private,
		})
	}
	budget, err := c.budget()
	if err != nil {
		return p, nil, err
	}
	p.TimeoutBudget = budget

	win, base, max := c.backoffDurations()
	health := NewHealthWindow(win, base, max)
	return p, health, nil
}

// budget parses TimeoutBudget, defaulting to disabled (0). A malformed value
// is an error: a silently dropped constraint would defeat the hard-budget
// guarantee the issue requires.
func (c Config) budget() (time.Duration, error) {
	s := strings.TrimSpace(c.TimeoutBudget)
	if s == "" || s == "0" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("fallback: parse timeout-budget %q: %w", s, err)
	}
	return d, nil
}

// backoffDurations resolves the health-window durations with defaults;
// unparseable optional values keep their defaults rather than erroring,
// because backoff tuning is best-effort, not a hard constraint.
func (c Config) backoffDurations() (window, base, max time.Duration) {
	window = c.bestEffortDur(c.BackoffWindow, 5*time.Minute)
	base = c.bestEffortDur(c.BackoffBase, 5*time.Second)
	max = c.bestEffortDur(c.BackoffMax, 5*time.Minute)
	return window, base, max
}

func (c Config) bestEffortDur(s string, def time.Duration) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return def
	}
	return d
}
