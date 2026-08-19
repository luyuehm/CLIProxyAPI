package costbudget

import (
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestPeriodWindows(t *testing.T) {
	mid := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)

	start, reset := PeriodWindow(PeriodMonthly, mid)
	wantStart := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	wantReset := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if !start.Equal(wantStart) || !reset.Equal(wantReset) {
		t.Fatalf("monthly window = [%s, %s), want [%s, %s)", start, reset, wantStart, wantReset)
	}

	start, reset = PeriodWindow(PeriodQuarterly, mid)
	// June is in Q2 (Apr-Jun)
	wantStart = time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	wantReset = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if !start.Equal(wantStart) || !reset.Equal(wantReset) {
		t.Fatalf("quarterly window = [%s, %s), want [%s, %s)", start, reset, wantStart, wantReset)
	}

	start, reset = PeriodWindow(PeriodAnnual, mid)
	wantStart = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	wantReset = time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	if !start.Equal(wantStart) || !reset.Equal(wantReset) {
		t.Fatalf("annual window = [%s, %s), want [%s, %s)", start, reset, wantStart, wantReset)
	}
}

func TestQuarterBoundaries(t *testing.T) {
	cases := []struct {
		t    time.Time
		want time.Time
	}{
		{time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 3, 31, 23, 59, 0, 0, time.UTC), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 12, 15, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		start, _ := PeriodWindow(PeriodQuarterly, c.t)
		if !start.Equal(c.want) {
			t.Fatalf("quarterStart(%s) = %s, want %s", c.t, start, c.want)
		}
	}
}

func TestCostForTokens(t *testing.T) {
	rate := &PriceRate{
		InputRatePer1K:      1.0,
		OutputRatePer1K:     2.0,
		CacheReadRatePer1K:  0.1,
		CacheWriteRatePer1K: 1.25,
		ReasoningRatePer1K:  3.0,
	}
	// 1000 uncached input, 200 cache-read, 100 cache-write,
	// 500 non-reasoning output, 300 reasoning.
	bd := coreusage.NewIndependentTokenBreakdown(1000, 200, 100, 500, 300, 2100)
	if !bd.Valid() {
		t.Fatalf("breakdown not valid: %+v", bd)
	}
	got := costForTokens(bd, rate)
	// 1.0 + 0.02 + 0.125 + 1.0 (non-reasoning) + 0.9 (reasoning) = 3.045
	want := 1.0 + 0.02 + 0.125 + 1.0 + 0.9
	if got != want {
		t.Fatalf("costForTokens = %v, want %v", got, want)
	}
}

func TestCostForTokensReasoningFallsBackToOutputRate(t *testing.T) {
	rate := &PriceRate{
		InputRatePer1K:  1.0,
		OutputRatePer1K: 2.0,
		// ReasoningRatePer1K left zero -> reasoning uses OutputRatePer1K
	}
	bd := coreusage.NewIndependentTokenBreakdown(0, 0, 0, 1000, 1000, 2000)
	got := costForTokens(bd, rate)
	// 2 (non-reasoning) + 2 (reasoning fallback) = 4
	if got != 4.0 {
		t.Fatalf("costForTokens reasoning fallback = %v, want 4", got)
	}
}

func TestPriceForFirstMatchWins(t *testing.T) {
	prices := []PriceRate{
		{Model: "gpt-4", InputRatePer1K: 1},
		{Model: "gpt-4", InputRatePer1K: 2}, // ignored: same model, later
		{Provider: "openai", InputRatePer1K: 3},
		{InputRatePer1K: 4}, // catch-all
	}
	if r := priceFor(prices, "openai", "gpt-4"); r.InputRatePer1K != 1 {
		t.Fatalf("specific model match = %v, want 1", r.InputRatePer1K)
	}
	if r := priceFor(prices, "openai", "gpt-5"); r.InputRatePer1K != 3 {
		t.Fatalf("provider-only match = %v, want 3", r.InputRatePer1K)
	}
	if r := priceFor(prices, "anthropic", "claude-3"); r.InputRatePer1K != 4 {
		t.Fatalf("catch-all match = %v, want 4", r.InputRatePer1K)
	}
	if r := priceFor(prices, "x", "y"); r.InputRatePer1K != 4 {
		t.Fatalf("no specific match -> catch-all = %v, want 4", r.InputRatePer1K)
	}
}

func TestPriceForNoMatch(t *testing.T) {
	if r := priceFor(nil, "openai", "gpt-4"); r != nil {
		t.Fatalf("priceFor(nil) = %v, want nil", r)
	}
	if r := priceFor([]PriceRate{{Model: "x", InputRatePer1K: 1}}, "openai", "gpt-4"); r != nil {
		t.Fatalf("priceFor no match = %v, want nil", r)
	}
}

func TestClassify(t *testing.T) {
	b := Budget{Amount: 100, WarnFraction: 0.8, CriticalFraction: 1.0}
	cases := []struct {
		util float64
		want AlertLevel
	}{
		{0.5, AlertNone},
		{0.79, AlertNone},
		{0.8, AlertWarning},
		{0.99, AlertWarning},
		{1.0, AlertCritical},
		{1.5, AlertCritical},
	}
	for _, c := range cases {
		if got := classify(c.util, b); got != c.want {
			t.Fatalf("classify(%v) = %v, want %v", c.util, got, c.want)
		}
	}
}

func TestClassifyDefaults(t *testing.T) {
	b := Budget{Amount: 100} // fractions zero -> defaults 0.8 / 1.0
	if classify(0.79, b) != AlertNone {
		t.Fatal("default warn should be 0.8")
	}
	if classify(0.8, b) != AlertWarning {
		t.Fatal("default warn should fire at 0.8")
	}
	if classify(1.0, b) != AlertCritical {
		t.Fatal("default critical should fire at 1.0")
	}
}
