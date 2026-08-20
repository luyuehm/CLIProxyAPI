package costallocation

import (
	"testing"
)

func TestRegistrySnapshots(t *testing.T) {
	// Test without active tracker
	Install(nil)
	emptyReport := ReportSnapshot()
	if emptyReport.Enabled {
		t.Fatal("expected empty report when disabled/nil")
	}

	emptySummary := SummarySnapshot()
	if len(emptySummary.DepartmentRankings) != 0 {
		t.Fatal("expected empty department rankings")
	}

	csvData, err := ExportSnapshotCSV(QueryOptions{})
	if err != nil {
		t.Fatalf("unexpected export error: %v", err)
	}
	if len(csvData) == 0 {
		t.Fatal("expected non-empty CSV header even when empty")
	}

	// Test with active tracker
	tracker := NewTracker(AllocationConfig{
		Enabled:  true,
		Currency: "USD",
	})
	Install(tracker)
	if Active() != tracker {
		t.Fatal("expected active tracker to match installed tracker")
	}

	activeReport := ReportSnapshot()
	if !activeReport.Enabled || activeReport.Currency != "USD" {
		t.Fatalf("expected enabled report with USD currency, got %+v", activeReport)
	}
}
