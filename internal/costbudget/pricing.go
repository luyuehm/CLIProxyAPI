package costbudget

import (
	"strings"

	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

// priceFor resolves the first matching PriceRate for the (provider, model)
// pair. The first entry whose Model and Provider both match (empty fields are
// wildcards) wins. Returns nil when nothing matches; callers then price at zero.
func priceFor(prices []PriceRate, provider, model string) *PriceRate {
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	for i := range prices {
		rate := &prices[i]
		if rate.Model != "" && !strings.EqualFold(rate.Model, model) {
			continue
		}
		if rate.Provider != "" && !strings.EqualFold(rate.Provider, provider) {
			continue
		}
		return rate
	}
	return nil
}

// costForTokens computes the spend for a token breakdown at the given rate.
// Tokens are charged per 1000 tokens. Reasoning tokens use ReasoningRatePer1K
// when > 0, otherwise the standard output rate.
func costForTokens(breakdown coreusage.TokenBreakdown, rate *PriceRate) float64 {
	if rate == nil {
		return 0
	}
	const perK = 1000.0

	inputTokens := breakdown.Input.UncachedTokens
	// Cache-write tokens are charged at the cache-write rate when present;
	// uncached tokens are the remainder charged at the input rate.
	cost := float64(inputTokens) / perK * rate.InputRatePer1K
	cost += float64(breakdown.Input.CacheReadTokens) / perK * rate.CacheReadRatePer1K
	cost += float64(breakdown.Input.CacheWriteTokens) / perK * rate.CacheWriteRatePer1K

	nonReasoning := breakdown.Output.NonReasoningTokens
	reasoning := breakdown.Output.ReasoningTokens
	cost += float64(nonReasoning) / perK * rate.OutputRatePer1K
	reasoningRate := rate.ReasoningRatePer1K
	if reasoningRate <= 0 {
		reasoningRate = rate.OutputRatePer1K
	}
	cost += float64(reasoning) / perK * reasoningRate
	return cost
}

// classify returns the alert level for the given utilization fraction against
// the budget's warn/critical thresholds (with their defaults applied).
func classify(utilization float64, b Budget) AlertLevel {
	warn := b.WarnFraction
	if warn <= 0 {
		warn = DefaultWarnFraction
	}
	critical := b.CriticalFraction
	if critical <= 0 {
		critical = DefaultCriticalFraction
	}
	if critical > 0 && utilization >= critical {
		return AlertCritical
	}
	if warn > 0 && utilization >= warn {
		return AlertWarning
	}
	return AlertNone
}

// fractionFor returns the threshold fraction that was crossed for the given
// level, so an Alert can report which line it tripped.
func fractionFor(level AlertLevel, b Budget) float64 {
	switch level {
	case AlertCritical:
		if b.CriticalFraction > 0 {
			return b.CriticalFraction
		}
		return DefaultCriticalFraction
	case AlertWarning:
		if b.WarnFraction > 0 {
			return b.WarnFraction
		}
		return DefaultWarnFraction
	default:
		return 0
	}
}
