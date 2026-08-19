package costbudget

import (
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func newTestRecord(model string, input, output int64, at time.Time) coreusage.Record {
	bd := coreusage.NewIndependentTokenBreakdown(input, 0, 0, output, 0, input+output)
	return coreusage.Record{
		Provider:    "openai",
		Model:       model,
		RequestedAt: at,
		Detail: coreusage.Detail{
			InputTokens:    bd.Input.TotalTokens,
			OutputTokens:   bd.Output.TotalTokens,
			TotalTokens:    bd.TotalTokens,
			TokenBreakdown: bd,
		},
	}
}

// apply pushes a record through the tracker without going through the usage
// manager, so tests run deterministically and synchronously.
func apply(t *testing.T, tr *Tracker, rec coreusage.Record) {
	t.Helper()
	tr.HandleUsage(nil, rec)
}

func TestTrackerFiresWarningThenCritical(t *testing.T) {
	var alerts []Alert
	alerter := AlerterFunc(func(a Alert) { alerts = append(alerts, a) })
	cfg := BudgetConfig{
		Enabled:  true,
		Currency: "USD",
		Budgets:  []Budget{{Period: PeriodMonthly, Amount: 100}},
		Prices:   []PriceRate{{Model: "gpt-4", InputRatePer1K: 1.0, OutputRatePer1K: 0}},
	}
	tr := NewTracker(cfg, alerter)
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)

	// 70k input tokens -> $70, below 0.8 warn threshold -> no alert.
	apply(t, tr, newTestRecord("gpt-4", 70000, 0, now))
	if len(alerts) != 0 {
		t.Fatalf("expected no alert below warn, got %d", len(alerts))
	}

	// +15k -> $85, crosses 0.8 warn -> one warning.
	apply(t, tr, newTestRecord("gpt-4", 15000, 0, now))
	if len(alerts) != 1 || alerts[0].Level != AlertWarning {
		t.Fatalf("expected one warning, got %+v", alerts)
	}

	// +5k -> $90, still warning band, no new alert (de-duped).
	apply(t, tr, newTestRecord("gpt-4", 5000, 0, now))
	if len(alerts) != 1 {
		t.Fatalf("expected still one alert (de-duped), got %d", len(alerts))
	}

	// +15k -> $105, crosses critical -> one critical.
	apply(t, tr, newTestRecord("gpt-4", 15000, 0, now))
	if len(alerts) != 2 || alerts[1].Level != AlertCritical {
		t.Fatalf("expected critical as 2nd alert, got %+v", alerts)
	}

	// More spend in critical band -> no additional alert.
	apply(t, tr, newTestRecord("gpt-4", 5000, 0, now))
	if len(alerts) != 2 {
		t.Fatalf("expected no third alert, got %d", len(alerts))
	}
}

func TestTrackerRolloverResetsSpendAndAlerts(t *testing.T) {
	var alerts []Alert
	alerter := AlerterFunc(func(a Alert) { alerts = append(alerts, a) })
	cfg := BudgetConfig{
		Enabled: true,
		Budgets: []Budget{{Period: PeriodMonthly, Amount: 10}},
		Prices:  []PriceRate{{InputRatePer1K: 1.0}}, // catch-all, $1/1k input
	}
	tr := NewTracker(cfg, alerter)
	june := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	// Spend $5 (5k tokens) -> 0.5 utilization, below the 0.8 warn threshold.
	apply(t, tr, newTestRecord("gpt-4", 5000, 0, june))
	if len(alerts) != 0 {
		t.Fatalf("expected no alert below warn, got %d", len(alerts))
	}

	// Add $4 -> $9 total (0.9), crosses warn(0.8) -> one warning.
	apply(t, tr, newTestRecord("gpt-4", 4000, 0, june))
	if len(alerts) != 1 || alerts[0].Level != AlertWarning {
		t.Fatalf("expected one warning, got %+v", alerts)
	}

	// Add $2 -> $11 total (1.1), crosses critical(1.0) -> one critical.
	apply(t, tr, newTestRecord("gpt-4", 2000, 0, june))
	if len(alerts) != 2 || alerts[1].Level != AlertCritical {
		t.Fatalf("expected warn then critical, got %+v", alerts)
	}

	// Move into July: spend resets, warning should fire again at 0.8.
	july := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	apply(t, tr, newTestRecord("gpt-4", 8000, 0, july)) // $8 -> 0.8 warn
	if len(alerts) != 3 || alerts[2].Level != AlertWarning {
		t.Fatalf("expected fresh warning after rollover, got %+v", alerts)
	}
	// Period of the new alert must reflect the July window.
	start := alerts[2].PeriodStart
	wantStart := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if !start.Equal(wantStart) {
		t.Fatalf("rollover alert periodStart = %s, want %s", start, wantStart)
	}
}

func TestTrackerSkipsFailedAndDisabledAndUnpriced(t *testing.T) {
	var alerts []Alert
	alerter := AlerterFunc(func(a Alert) { alerts = append(alerts, a) })

	// Disabled: no alerts, report shows disabled.
	tr := NewTracker(BudgetConfig{Enabled: false, Budgets: []Budget{{Amount: 1}}}, alerter)
	tr.HandleUsage(nil, newTestRecord("gpt-4", 1_000_000, 0, time.Now()))
	if len(alerts) != 0 {
		t.Fatal("disabled tracker must not alert")
	}
	if tr.Report().Enabled {
		t.Fatal("disabled tracker report must show enabled=false")
	}

	// Failed record is skipped even when enabled.
	tr2 := NewTracker(BudgetConfig{Enabled: true, Budgets: []Budget{{Amount: 1}}, Prices: []PriceRate{{InputRatePer1K: 1}}}, alerter)
	rec := newTestRecord("gpt-4", 1000, 0, time.Now())
	rec.Failed = true
	tr2.HandleUsage(nil, rec)
	if len(alerts) != 0 {
		t.Fatal("failed record must not accrue spend")
	}

	// No price match -> spend is 0, no alert.
	tr3 := NewTracker(BudgetConfig{Enabled: true, Budgets: []Budget{{Amount: 1}}, Prices: nil}, alerter)
	tr3.HandleUsage(nil, newTestRecord("gpt-4", 1_000_000, 0, time.Now()))
	if len(alerts) != 0 {
		t.Fatal("unpriced model must not accrue spend")
	}
}

func TestReportReflectsState(t *testing.T) {
	cfg := BudgetConfig{
		Enabled:  true,
		Currency: "CNY",
		Budgets: []Budget{
			{Period: PeriodMonthly, Amount: 100, WarnFraction: 0.8, CriticalFraction: 1.0},
			{Period: PeriodAnnual, Amount: 1000, WarnFraction: 0.5, CriticalFraction: 0.9},
		},
		Prices: []PriceRate{{InputRatePer1K: 1.0}},
	}
	tr := NewTracker(cfg, nil)
	now := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	// $85 spend: both budgets accrue every record. Monthly -> 0.85 (warning);
	// annual -> 0.085 (none).
	apply(t, tr, newTestRecord("gpt-4", 85000, 0, now))

	rep := tr.Report()
	if !rep.Enabled || rep.Currency != "CNY" {
		t.Fatalf("report header wrong: %+v", rep)
	}
	if len(rep.Budgets) != 2 {
		t.Fatalf("expected 2 budget entries, got %d", len(rep.Budgets))
	}
	m := rep.Budgets[0]
	if m.Level != AlertWarning || m.CurrentSpend != 85 || m.Utilization != 0.85 {
		t.Fatalf("monthly entry wrong: %+v", m)
	}
	wantStart := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if !m.PeriodStart.Equal(wantStart) {
		t.Fatalf("monthly periodStart = %s, want %s", m.PeriodStart, wantStart)
	}
	a := rep.Budgets[1]
	// Annual accrues the same $85 -> 0.085, below the 0.5 warn threshold.
	if a.Level != AlertNone || a.CurrentSpend != 85 || a.Utilization != 0.085 {
		t.Fatalf("annual entry wrong: %+v", a)
	}
	wantAnnualStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if !a.PeriodStart.Equal(wantAnnualStart) {
		t.Fatalf("annual periodStart = %s, want %s", a.PeriodStart, wantAnnualStart)
	}
}

func TestMultipleBudgetsSamePeriodIndependent(t *testing.T) {
	var alerts []Alert
	alerter := AlerterFunc(func(a Alert) { alerts = append(alerts, a) })
	// Two monthly budgets: tight ($10) and loose ($1000), same period.
	cfg := BudgetConfig{
		Enabled: true,
		Budgets: []Budget{
			{Period: PeriodMonthly, Amount: 10},
			{Period: PeriodMonthly, Amount: 1000},
		},
		Prices: []PriceRate{{InputRatePer1K: 1.0}},
	}
	tr := NewTracker(cfg, alerter)
	now := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	// $8 spend: tight budget ($10) at 0.8 -> warning; loose ($1000) at 0.008 -> none.
	apply(t, tr, newTestRecord("gpt-4", 8000, 0, now))
	if len(alerts) != 1 || alerts[0].Level != AlertWarning {
		t.Fatalf("expected one warning from tight budget, got %d: %+v", len(alerts), alerts)
	}
	// +$3 -> $11 total, tight budget crosses critical(1.0); loose at 0.011 still none.
	apply(t, tr, newTestRecord("gpt-4", 3000, 0, now))
	if len(alerts) != 2 || alerts[1].Level != AlertCritical {
		t.Fatalf("expected warn then critical from tight budget, got %+v", alerts)
	}
	rep := tr.Report()
	if rep.Budgets[1].Level != AlertNone {
		t.Fatalf("loose budget should be at none, got %v", rep.Budgets[1].Level)
	}
}
