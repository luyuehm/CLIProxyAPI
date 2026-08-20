package costallocation

import "time"

// DepartmentReport is the per-department entry in the cost allocation report.
type DepartmentReport struct {
	Name         string  `json:"name"`
	TotalSpend   float64 `json:"total_spend"`
	RequestCount int64   `json:"request_count"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	TotalTokens  int64   `json:"total_tokens"`
}

// PeriodSummary holds a period's worth of aggregated data for one department.
type PeriodSummary struct {
	PeriodKey    string  `json:"period_key"`
	Spend        float64 `json:"spend"`
	RequestCount int64   `json:"request_count"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	TotalTokens  int64   `json:"total_tokens"`
}

// CostAllocationReport is the snapshot returned by the report endpoint.
type CostAllocationReport struct {
	Enabled     bool               `json:"enabled"`
	Currency    string             `json:"currency,omitempty"`
	Period      Period             `json:"period"`
	GeneratedAt time.Time          `json:"generated_at"`
	Departments []DepartmentReport `json:"departments,omitempty"`
}

// Report returns a point-in-time snapshot of every department's current window.
func (t *Tracker) Report() CostAllocationReport {
	report := CostAllocationReport{
		GeneratedAt: time.Now(),
	}
	if t == nil {
		return report
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	report.Enabled = t.cfg.Enabled
	report.Currency = t.cfg.Currency
	report.Period = t.cfg.effectivePeriod()

	for name, state := range t.departments {
		dept := DepartmentReport{
			Name:         name,
			TotalSpend:   state.CurrentSpend,
			RequestCount: state.CurrentRequestCount,
			InputTokens:  state.CurrentInputTokens,
			OutputTokens: state.CurrentOutputTokens,
			TotalTokens:  state.CurrentTotalTokens,
		}
		report.Departments = append(report.Departments, dept)
	}
	if report.Departments == nil {
		report.Departments = []DepartmentReport{}
	}
	return report
}

// PeriodSummaryReport returns a per-period breakdown for each department,
// combining current window data with historical buckets.
func (t *Tracker) PeriodSummaryReport() ([]DepartmentPeriodSummary, error) {
	summaries := []DepartmentPeriodSummary{}
	if t == nil || !t.Enabled() {
		return summaries, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	period := t.cfg.effectivePeriod()

	for name, state := range t.departments {
		dps := DepartmentPeriodSummary{
			DepartmentName: name,
		}
		// Historical buckets
		for _, b := range state.Buckets {
			dps.Periods = append(dps.Periods, PeriodSummary{
				PeriodKey:    b.PeriodKey,
				Spend:        b.Spend,
				RequestCount: b.RequestCount,
				InputTokens:  b.InputTokens,
				OutputTokens: b.OutputTokens,
				TotalTokens:  b.TotalTokens,
			})
		}
		// Current window (if there is data)
		if state.CurrentRequestCount > 0 {
			dps.Periods = append(dps.Periods, PeriodSummary{
				PeriodKey:    PeriodKey(period, state.CurrentPeriodStart),
				Spend:        state.CurrentSpend,
				RequestCount: state.CurrentRequestCount,
				InputTokens:  state.CurrentInputTokens,
				OutputTokens: state.CurrentOutputTokens,
				TotalTokens:  state.CurrentTotalTokens,
			})
		}
		if len(dps.Periods) > 0 {
			summaries = append(summaries, dps)
		}
	}
	return summaries, nil
}

// DepartmentPeriodSummary groups a department's period summaries.
type DepartmentPeriodSummary struct {
	DepartmentName string          `json:"department_name"`
	Periods        []PeriodSummary `json:"periods"`
}
