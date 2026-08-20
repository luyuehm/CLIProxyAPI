package costallocation

import (
	"testing"

	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestPricingResolutionAndCalculation(t *testing.T) {
	prices := []PriceRate{
		{
			Provider:            "openai",
			Model:               "gpt-4o",
			InputRatePer1K:      0.005,
			OutputRatePer1K:     0.015,
			CacheReadRatePer1K:  0.0025,
			CacheWriteRatePer1K: 0.005,
			ReasoningRatePer1K:  0.020,
		},
		{
			Provider:        "anthropic",
			Model:           "claude-3-5-sonnet",
			InputRatePer1K:  0.003,
			OutputRatePer1K: 0.015,
		},
		{
			// Wildcard default
			InputRatePer1K:  0.001,
			OutputRatePer1K: 0.002,
		},
	}

	rate := priceFor(prices, "openai", "gpt-4o")
	if rate == nil || rate.InputRatePer1K != 0.005 {
		t.Fatalf("expected gpt-4o rate, got %+v", rate)
	}

	wildcardRate := priceFor(prices, "custom", "custom-model")
	if wildcardRate == nil || wildcardRate.InputRatePer1K != 0.001 {
		t.Fatalf("expected wildcard rate, got %+v", wildcardRate)
	}

	breakdown := coreusage.TokenBreakdown{
		Input: coreusage.TokenInputBreakdown{
			UncachedTokens:   1000,
			CacheReadTokens:  2000,
			CacheWriteTokens: 500,
		},
		Output: coreusage.TokenOutputBreakdown{
			NonReasoningTokens: 1000,
			ReasoningTokens:    500,
		},
		TotalTokens: 4500,
	}

	cost := costForTokens(breakdown, rate)
	expected := 0.0375
	if cost < expected-1e-6 || cost > expected+1e-6 {
		t.Fatalf("expected cost %f, got %f", expected, cost)
	}
}
