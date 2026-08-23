package costbudget

import (
	"testing"
	"time"

	"cpa-usage-keeper/internal/entities"
)

func TestValidPeriod(t *testing.T) {
	cases := []struct {
		period entities.BudgetPeriod
		want   bool
	}{
		{entities.BudgetPeriodMonthly, true},
		{entities.BudgetPeriodQuarterly, true},
		{entities.BudgetPeriodYearly, true},
		{entities.BudgetPeriod("weekly"), false},
		{entities.BudgetPeriod(""), false},
	}
	for _, tc := range cases {
		if got := validPeriod(tc.period); got != tc.want {
			t.Fatalf("validPeriod(%q) = %v, want %v", tc.period, got, tc.want)
		}
	}
}

func TestPeriodRangeMonthly(t *testing.T) {
	now := time.Date(2026, time.August, 20, 14, 30, 0, 0, time.Local)
	start, end := periodRange(entities.BudgetPeriodMonthly, now)
	wantStart := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.Local)
	if !start.Equal(wantStart) {
		t.Fatalf("monthly start = %v, want %v", start, wantStart)
	}
	wantEnd := time.Date(2026, time.August, 31, 23, 59, 59, 999999999, time.Local)
	if !end.Equal(wantEnd) {
		t.Fatalf("monthly end = %v, want %v", end, wantEnd)
	}
}

func TestPeriodRangeQuarterly(t *testing.T) {
	now := time.Date(2026, time.November, 15, 10, 0, 0, 0, time.Local)
	start, end := periodRange(entities.BudgetPeriodQuarterly, now)
	wantStart := time.Date(2026, time.October, 1, 0, 0, 0, 0, time.Local)
	if !start.Equal(wantStart) {
		t.Fatalf("quarterly start = %v, want %v", start, wantStart)
	}
	wantEnd := time.Date(2026, time.December, 31, 23, 59, 59, 999999999, time.Local)
	if !end.Equal(wantEnd) {
		t.Fatalf("quarterly end = %v, want %v", end, wantEnd)
	}
}

func TestPeriodRangeYearly(t *testing.T) {
	now := time.Date(2026, time.June, 1, 0, 0, 0, 0, time.Local)
	start, end := periodRange(entities.BudgetPeriodYearly, now)
	wantStart := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.Local)
	if !start.Equal(wantStart) {
		t.Fatalf("yearly start = %v, want %v", start, wantStart)
	}
	wantEnd := time.Date(2026, time.December, 31, 23, 59, 59, 999999999, time.Local)
	if !end.Equal(wantEnd) {
		t.Fatalf("yearly end = %v, want %v", end, wantEnd)
	}
}

func TestToConfigView(t *testing.T) {
	config := &entities.BudgetConfig{
		ID:             1,
		Period:         entities.BudgetPeriodMonthly,
		Amount:         100,
		AlertThreshold: 80,
		AlertEnabled:   true,
		AlertFired:     false,
		PeriodStart:    time.Date(2026, time.August, 1, 0, 0, 0, 0, time.Local),
		PeriodEnd:      time.Date(2026, time.August, 31, 23, 59, 59, 999999999, time.Local),
	}
	view := toConfigView(config)
	if view.Period != entities.BudgetPeriodMonthly {
		t.Fatalf("view period = %q, want monthly", view.Period)
	}
	if view.Amount != 100 {
		t.Fatalf("view amount = %v, want 100", view.Amount)
	}
	if view.Currency != "USD" {
		t.Fatalf("view currency = %q, want USD", view.Currency)
	}
	if view.AlertThreshold != 80 {
		t.Fatalf("view alert threshold = %v, want 80", view.AlertThreshold)
	}
}
