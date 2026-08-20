package costallocation

import (
	"testing"
	"time"
)

func TestPeriodWindowAndKey(t *testing.T) {
	ts := time.Date(2026, 8, 20, 14, 30, 0, 0, time.UTC)

	// Daily
	dailyKey := PeriodKey(PeriodDaily, ts)
	if dailyKey != "2026-08-20" {
		t.Fatalf("expected daily key 2026-08-20, got %s", dailyKey)
	}
	dStart, dEnd := PeriodWindow(PeriodDaily, ts)
	if dStart.Day() != 20 || dEnd.Day() != 21 {
		t.Fatalf("unexpected daily window: %v to %v", dStart, dEnd)
	}

	// Monthly
	monthlyKey := PeriodKey(PeriodMonthly, ts)
	if monthlyKey != "2026-08" {
		t.Fatalf("expected monthly key 2026-08, got %s", monthlyKey)
	}
	mStart, mEnd := PeriodWindow(PeriodMonthly, ts)
	if mStart.Month() != time.August || mEnd.Month() != time.September {
		t.Fatalf("unexpected monthly window: %v to %v", mStart, mEnd)
	}

	// Quarterly
	qKey := PeriodKey(PeriodQuarterly, ts)
	if qKey != "2026-Q3" {
		t.Fatalf("expected quarterly key 2026-Q3, got %s", qKey)
	}
	qStart, qEnd := PeriodWindow(PeriodQuarterly, ts)
	if qStart.Month() != time.July || qEnd.Month() != time.October {
		t.Fatalf("unexpected quarterly window: %v to %v", qStart, qEnd)
	}

	// Annual
	aKey := PeriodKey(PeriodAnnual, ts)
	if aKey != "2026" {
		t.Fatalf("expected annual key 2026, got %s", aKey)
	}
	aStart, aEnd := PeriodWindow(PeriodAnnual, ts)
	if aStart.Year() != 2026 || aEnd.Year() != 2027 {
		t.Fatalf("unexpected annual window: %v to %v", aStart, aEnd)
	}

	// All time
	allKey := PeriodKey(PeriodAllTime, ts)
	if allKey != "all" {
		t.Fatalf("expected all time key all, got %s", allKey)
	}
}
