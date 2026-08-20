package costallocation

import (
	"fmt"
	"time"
)

// Period represents the time aggregation window.
type Period string

const (
	PeriodDaily     Period = "daily"
	PeriodMonthly   Period = "monthly"
	PeriodQuarterly Period = "quarterly"
	PeriodAnnual    Period = "annual"
	PeriodAllTime   Period = "all_time"
)

// PeriodKey returns the formatted period key for a given timestamp and period type.
// E.g. "2026-08-20" for daily, "2026-08" for monthly, "2026-Q3" for quarterly, "2026" for annual, "all" for all_time.
func PeriodKey(period Period, t time.Time) string {
	if t.IsZero() {
		t = time.Now()
	}
	utc := t.UTC()
	switch period {
	case PeriodDaily:
		return utc.Format("2006-01-02")
	case PeriodQuarterly:
		q := (int(utc.Month()-1) / 3) + 1
		return fmt.Sprintf("%04d-Q%d", utc.Year(), q)
	case PeriodAnnual:
		return fmt.Sprintf("%04d", utc.Year())
	case PeriodAllTime:
		return "all"
	default: // PeriodMonthly
		return utc.Format("2006-01")
	}
}

// PeriodWindow returns the [start, resetAt) window for the given period containing t.
func PeriodWindow(period Period, t time.Time) (start, resetAt time.Time) {
	if t.IsZero() {
		t = time.Now()
	}
	utc := t.UTC()
	switch period {
	case PeriodDaily:
		start = time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
		resetAt = start.AddDate(0, 0, 1)
	case PeriodQuarterly:
		month := int(utc.Month()-time.January) / 3 * 3
		start = time.Date(utc.Year(), time.January+time.Month(month), 1, 0, 0, 0, 0, time.UTC)
		resetAt = start.AddDate(0, 3, 0)
	case PeriodAnnual:
		start = time.Date(utc.Year(), time.January, 1, 0, 0, 0, 0, time.UTC)
		resetAt = start.AddDate(1, 0, 0)
	case PeriodAllTime:
		start = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
		resetAt = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
	default: // PeriodMonthly
		start = time.Date(utc.Year(), utc.Month(), 1, 0, 0, 0, 0, time.UTC)
		resetAt = start.AddDate(0, 1, 0)
	}
	return start, resetAt
}
