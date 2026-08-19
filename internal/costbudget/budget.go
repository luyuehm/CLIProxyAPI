// Package costbudget tracks provider request spend against configurable
// monthly, quarterly, and annual budgets and emits alerts when configured
// thresholds are crossed. It plugs into the existing usage accounting pipeline
// as a usage.Plugin, exactly like the redisqueue usage plugin.
package costbudget

import "time"

// Period describes the time window over which a budget is enforced.
type Period string

const (
	// PeriodMonthly resets every calendar month.
	PeriodMonthly Period = "monthly"
	// PeriodQuarterly resets every calendar quarter (Jan/Apr/Jul/Oct).
	PeriodQuarterly Period = "quarterly"
	// PeriodAnnual resets every calendar year.
	PeriodAnnual Period = "annual"
)

// AlertLevel classifies how far spend has progressed relative to a budget.
type AlertLevel string

const (
	// AlertNone means spend is below the warning threshold.
	AlertNone AlertLevel = "none"
	// AlertWarning means spend crossed the warning threshold but not critical.
	AlertWarning AlertLevel = "warning"
	// AlertCritical means spend crossed the critical threshold.
	AlertCritical AlertLevel = "critical"
)

// Default thresholds expressed as fractions of the configured budget (0..1).
const (
	DefaultWarnFraction     = 0.8
	DefaultCriticalFraction = 1.0
)

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

// Budget defines a single spend ceiling for a period.
type Budget struct {
	// Period is the time window over which the budget is enforced.
	Period Period `yaml:"period,omitempty" json:"period,omitempty"`
	// Amount is the spend ceiling in the configured currency unit.
	Amount float64 `yaml:"amount,omitempty" json:"amount,omitempty"`
	// WarnFraction is the fraction of Amount at which a warning alert fires.
	// Defaults to 0.8 when <= 0.
	WarnFraction float64 `yaml:"warn-fraction,omitempty" json:"warn-fraction,omitempty"`
	// CriticalFraction is the fraction of Amount at which a critical alert fires.
	// Defaults to 1.0 when <= 0.
	CriticalFraction float64 `yaml:"critical-fraction,omitempty" json:"critical-fraction,omitempty"`
}

// BudgetConfig is the user-facing configuration for cost budgeting.
type BudgetConfig struct {
	// Enabled toggles the cost budget tracker. When false, usage records are
	// not aggregated for budgeting and the report endpoint returns an empty state.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Currency is the free-form currency unit for Amount and reported spend
	// (e.g. "USD", "CNY"). Purely informational.
	Currency string `yaml:"currency,omitempty" json:"currency,omitempty"`
	// Budgets is the set of spend ceilings to track simultaneously. Each entry
	// is evaluated independently against the same aggregated spend.
	Budgets []Budget `yaml:"budgets,omitempty" json:"budgets,omitempty"`
	// Prices is the ordered price table. The first matching entry for a given
	// (provider, model) pair wins; a later, more specific entry does NOT override
	// an earlier broader one, so list specific rates first.
	Prices []PriceRate `yaml:"prices,omitempty" json:"prices,omitempty"`
}

// Alert describes a budget threshold crossing at a point in time.
type Alert struct {
	Period            Period     `json:"period"`
	Level             AlertLevel `json:"level"`
	BudgetAmount      float64    `json:"budget_amount"`
	CurrentSpend      float64    `json:"current_spend"`
	Utilization       float64    `json:"utilization"`
	PeriodStart       time.Time  `json:"period_start"`
	PeriodResetAt     time.Time  `json:"period_reset_at"`
	TriggeredAt       time.Time  `json:"triggered_at"`
	ThresholdFraction float64    `json:"threshold_fraction"`
}

// Alerter receives budget alerts. Implementations are expected to forward the
// alert to an external notification channel (webhook, email, etc.).
type Alerter interface {
	Notify(alert Alert)
}

// alerterFunc adapts a plain function to the Alerter interface.
type alerterFunc struct {
	fn func(Alert)
}

func (a alerterFunc) Notify(alert Alert) {
	if a.fn != nil {
		a.fn(alert)
	}
}

// AlerterFunc wraps a plain function as an Alerter.
func AlerterFunc(fn func(Alert)) Alerter { return alerterFunc{fn: fn} }
