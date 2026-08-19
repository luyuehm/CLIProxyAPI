package costbudget

import "time"

// PeriodWindow returns the [start, resetAt) window for the given period that
// contains t. resetAt is the exclusive upper bound (the start of the next window).
func PeriodWindow(period Period, t time.Time) (start, resetAt time.Time) {
	if t.IsZero() {
		t = time.Now()
	}
	utc := t.UTC()
	switch period {
	case PeriodQuarterly:
		start = quarterStart(utc)
		resetAt = start.AddDate(0, 3, 0)
	case PeriodAnnual:
		start = time.Date(utc.Year(), time.January, 1, 0, 0, 0, 0, time.UTC)
		resetAt = start.AddDate(1, 0, 0)
	default: // PeriodMonthly and anything unrecognized
		start = time.Date(utc.Year(), utc.Month(), 1, 0, 0, 0, 0, time.UTC)
		resetAt = start.AddDate(0, 1, 0)
		if period != PeriodMonthly {
			// Unknown period -> treat as monthly so budgeting still works safely.
		}
	}
	return start, resetAt
}

// quarterStart returns the UTC midnight of the first day of the quarter containing t.
func quarterStart(t time.Time) time.Time {
	month := int(t.Month()-time.January) / 3 * 3 // 0,3,6,9
	return time.Date(t.Year(), time.January+time.Month(month), 1, 0, 0, 0, 0, time.UTC)
}

// PeriodStart returns the start of the window for period containing t.
func PeriodStart(period Period, t time.Time) time.Time {
	start, _ := PeriodWindow(period, t)
	return start
}

// PeriodResetAt returns the exclusive end of the window for period containing t.
func PeriodResetAt(period Period, t time.Time) time.Time {
	_, reset := PeriodWindow(period, t)
	return reset
}
