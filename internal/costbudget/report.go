package costbudget

import "time"

// BudgetReport is the snapshot returned by the report endpoint. It carries
// per-budget current spend, utilization, and alert level for the active window.
type BudgetReport struct {
	// Enabled reports whether the tracker is active.
	Enabled bool `json:"enabled"`
	// Currency is the configured currency unit for the reported figures.
	Currency string `json:"currency,omitempty"`
	// GeneratedAt is when the report snapshot was taken.
	GeneratedAt time.Time `json:"generated_at"`
	// Budgets is one entry per configured budget, in config order.
	Budgets []BudgetReportEntry `json:"budgets"`
}

// BudgetReportEntry is a single budget's current state.
type BudgetReportEntry struct {
	Period       Period  `json:"period"`
	BudgetAmount float64 `json:"budget_amount"`
	CurrentSpend float64 `json:"current_spend"`
	// Utilization is CurrentSpend / BudgetAmount, or 0 when the budget is 0.
	Utilization   float64    `json:"utilization"`
	Level         AlertLevel `json:"level"`
	PeriodStart   time.Time  `json:"period_start"`
	PeriodResetAt time.Time  `json:"period_reset_at"`
}

// Report returns a point-in-time snapshot of every budget's current window.
//
// The snapshot reflects the tracker's own accrued window state (the spend and
// window boundaries it is currently charging against). It deliberately does
// not recompute the window against the wall clock: if the process is mid-period,
// the reported window and spend are internally consistent, even if the wall
// clock has, say, just crossed midnight UTC and no new request has arrived yet
// to trigger rollover.
func (t *Tracker) Report() BudgetReport {
	report := BudgetReport{
		GeneratedAt: time.Now(),
	}
	if t == nil {
		return report
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	report.Enabled = t.cfg.Enabled
	report.Currency = t.cfg.Currency
	report.Budgets = make([]BudgetReportEntry, len(t.spend))
	for i := range t.spend {
		ps := t.spend[i]
		budget := t.cfg.Budgets[i]
		entry := BudgetReportEntry{
			Period:        budget.Period,
			BudgetAmount:  budget.Amount,
			CurrentSpend:  ps.totalSpend,
			PeriodStart:   ps.start,
			PeriodResetAt: ps.resetAt,
		}
		if budget.Amount > 0 {
			utilization := ps.totalSpend / budget.Amount
			entry.Utilization = utilization
			// The reported level is the live classification of the current
			// spend, which is at least as high as the last level an alert
			// fired for (alerts de-dupe, but the report reflects reality).
			entry.Level = classify(utilization, budget)
		}
		report.Budgets[i] = entry
	}
	return report
}
