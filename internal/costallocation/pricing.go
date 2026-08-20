package costallocation

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

// costForTokens computes the cost for a token breakdown at the given rate.
// Tokens are charged per 1000 tokens. Reasoning tokens use ReasoningRatePer1K
// when > 0, otherwise the standard output rate.
func costForTokens(breakdown coreusage.TokenBreakdown, rate *PriceRate) float64 {
	if rate == nil {
		return 0
	}
	const perK = 1000.0

	cost := float64(breakdown.Input.UncachedTokens) / perK * rate.InputRatePer1K
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
