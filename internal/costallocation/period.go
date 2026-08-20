package costallocation

import (
	"strconv"
	"time"
)

// PeriodWindow returns the start of the period window containing t and the
// reset time (start of the next period). For monthly periods the window starts
// at the beginning of the calendar month; for quarterly it starts at the
// beginning of the containing quarter (Jan 1, Apr 1, Jul 1, Oct 1).
func PeriodWindow(p Period, t time.Time) (start, resetAt time.Time) {
	year, month, _ := t.Date()
	switch p {
	case PeriodQuarterly:
		quarterStart := time.Month(((int(month)-1)/3)*3 + 1)
		start = time.Date(year, quarterStart, 1, 0, 0, 0, 0, t.Location())
		nextQuarter := time.Month(((int(month)-1)/3+1)*3 + 1)
		if nextQuarter > time.December {
			nextQuarter = time.January
			year++
		}
		resetAt = time.Date(year, nextQuarter, 1, 0, 0, 0, 0, t.Location())
	default: // monthly
		start = time.Date(year, month, 1, 0, 0, 0, 0, t.Location())
		nextMonth := month + 1
		if nextMonth > time.December {
			nextMonth = time.January
			year++
		}
		resetAt = time.Date(year, nextMonth, 1, 0, 0, 0, 0, t.Location())
	}
	return
}

// PeriodKey returns a human-readable period key string (e.g. "2026-03" for
// monthly, "2026-Q1" for quarterly).
func PeriodKey(p Period, t time.Time) string {
	year, month, _ := t.Date()
	if p == PeriodQuarterly {
		q := (int(month)-1)/3 + 1
		return strconv.Itoa(year) + "-Q" + strconv.Itoa(q)
	}
	return strconv.Itoa(year) + "-" + twoDigits(int(month))
}

func twoDigits(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}
