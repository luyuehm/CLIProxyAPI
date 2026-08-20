package costallocation

import (
	"context"
	"math"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestAllocateDepartment(t *testing.T) {
	rules := []DepartmentRule{
		{Name: "engineering", APIKeys: []string{"sk-eng-001", "sk-eng-002"}},
		{Name: "engineering", APIKeyPrefixes: []string{"sk-eng-"}},
		{Name: "finance", APIKeyPrefixes: []string{"sk-fin-"}},
		{Name: "research", AuthIDs: []string{"auth-research-01"}},
		{Name: "default-team", Default: true},
	}

	tests := []struct {
		name   string
		record coreusage.Record
		want   string
	}{
		{"exact api key match", coreusage.Record{APIKey: "sk-eng-001"}, "engineering"},
		{"prefix match engineering", coreusage.Record{APIKey: "sk-eng-alpha"}, "engineering"},
		{"prefix match finance", coreusage.Record{APIKey: "sk-fin-xxx"}, "finance"},
		{"auth id match", coreusage.Record{AuthID: "auth-research-01"}, "research"},
		{"fallback to default", coreusage.Record{APIKey: "sk-unknown-01"}, "default-team"},
		{"fallback no match", coreusage.Record{APIKey: "other"}, "default-team"},
		{"empty api key", coreusage.Record{APIKey: ""}, "default-team"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AllocateDepartment(rules, "unallocated", tt.record)
			if got != tt.want {
				t.Errorf("AllocateDepartment() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAllocateDepartment_UnallocatedFallback(t *testing.T) {
	// No default rule -> use unallocatedName
	rules := []DepartmentRule{
		{Name: "engineering", APIKeys: []string{"sk-eng-001"}},
	}
	got := AllocateDepartment(rules, "unallocated", coreusage.Record{APIKey: "unknown"})
	if got != "unallocated" {
		t.Errorf("expected unallocated, got %q", got)
	}
}

func TestTrackerEnabled(t *testing.T) {
	tracker := NewTracker(CostAllocationConfig{Enabled: true})
	if !tracker.Enabled() {
		t.Error("expected enabled")
	}
	tracker2 := NewTracker(CostAllocationConfig{Enabled: false})
	if tracker2.Enabled() {
		t.Error("expected disabled")
	}
	var nilTracker *Tracker
	if nilTracker.Enabled() {
		t.Error("nil tracker should be disabled")
	}
}

func TestHandleUsage(t *testing.T) {
	cfg := CostAllocationConfig{
		Enabled:  true,
		Currency: "USD",
		Rules: []DepartmentRule{
			{Name: "engineering", APIKeyPrefixes: []string{"sk-eng-"}},
			{Name: "it", Default: true},
		},
		Prices: []PriceRate{
			{InputRatePer1K: 1.0, OutputRatePer1K: 2.0, CacheReadRatePer1K: 0.5},
		},
	}
	tracker := NewTracker(cfg)

	record := coreusage.Record{
		APIKey:      "sk-eng-test",
		Provider:    "openai",
		Model:       "gpt-5.4",
		RequestedAt: time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC),
		Detail: coreusage.Detail{
			TokenBreakdown: coreusage.TokenBreakdown{
				SchemaVersion: 2,
				Quality:       coreusage.TokenAccountingQualityComplete,
				TotalTokens:   1000,
				Input: coreusage.TokenInputBreakdown{
					TotalTokens:     500,
					UncachedTokens:  300,
					CacheReadTokens: 200,
				},
				Output: coreusage.TokenOutputBreakdown{
					TotalTokens:        500,
					NonReasoningTokens: 400,
					ReasoningTokens:    100,
				},
			},
		},
	}

	tracker.HandleUsage(context.Background(), record)

	report := tracker.Report()
	if !report.Enabled {
		t.Fatal("report should be enabled")
	}
	if len(report.Departments) != 1 {
		t.Fatalf("expected 1 department, got %d", len(report.Departments))
	}
	if report.Departments[0].Name != "engineering" {
		t.Errorf("expected engineering, got %s", report.Departments[0].Name)
	}
	if report.Departments[0].RequestCount != 1 {
		t.Errorf("expected 1 request, got %d", report.Departments[0].RequestCount)
	}
	// Input: 300 uncached @ 1.0/1k = 0.3, 200 cached @ 0.5/1k = 0.1; Output: 400 @ 2.0/1k = 0.8, 100 reasoning @ 2.0/1k = 0.2
	// Total = 0.3 + 0.1 + 0.8 + 0.2 = 1.4
	expectedSpend := 1.4
	if math.Abs(report.Departments[0].TotalSpend-expectedSpend) > 1e-9 {
		t.Errorf("expected spend %.6f, got %.6f", expectedSpend, report.Departments[0].TotalSpend)
	}
}

func TestHandleUsage_FailedRecord(t *testing.T) {
	cfg := CostAllocationConfig{
		Enabled: true,
		Rules: []DepartmentRule{
			{Name: "eng", APIKeyPrefixes: []string{"sk-eng-"}},
		},
	}
	tracker := NewTracker(cfg)

	record := coreusage.Record{
		APIKey: "sk-eng-test",
		Failed: true,
		Detail: coreusage.Detail{
			TokenBreakdown: coreusage.TokenBreakdown{
				SchemaVersion: 2,
				Quality:       coreusage.TokenAccountingQualityComplete,
				TotalTokens:   100,
				Input:         coreusage.TokenInputBreakdown{TotalTokens: 50, UncachedTokens: 50},
				Output:        coreusage.TokenOutputBreakdown{TotalTokens: 50, NonReasoningTokens: 50},
			},
		},
	}
	tracker.HandleUsage(context.Background(), record)
	report := tracker.Report()
	if len(report.Departments) != 0 {
		t.Error("failed records should not be counted")
	}
}

func TestTracker_PeriodRollover(t *testing.T) {
	cfg := CostAllocationConfig{
		Enabled: true,
		Period:  PeriodMonthly,
		Rules:   []DepartmentRule{{Name: "eng", APIKeyPrefixes: []string{"sk-"}}},
		Prices:  []PriceRate{{InputRatePer1K: 1.0, OutputRatePer1K: 1.0}},
	}
	tracker := NewTracker(cfg)

	// Record in August.
	augRecord := coreusage.Record{
		APIKey:      "sk-test",
		RequestedAt: time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC),
		Detail: coreusage.Detail{
			TokenBreakdown: coreusage.TokenBreakdown{
				SchemaVersion: 2,
				Quality:       coreusage.TokenAccountingQualityComplete,
				TotalTokens:   1000,
				Input:         coreusage.TokenInputBreakdown{TotalTokens: 500, UncachedTokens: 500},
				Output:        coreusage.TokenOutputBreakdown{TotalTokens: 500, NonReasoningTokens: 500},
			},
		},
	}
	tracker.HandleUsage(context.Background(), augRecord)

	// Record in September (triggers rollover).
	sepRecord := augRecord
	sepRecord.RequestedAt = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	tracker.HandleUsage(context.Background(), sepRecord)

	report := tracker.Report()
	if len(report.Departments) != 1 {
		t.Fatalf("expected 1 department, got %d", len(report.Departments))
	}
	dept := report.Departments[0]
	if dept.RequestCount != 1 {
		t.Errorf("current window should have 1 request (sep), got %d", dept.RequestCount)
	}
	if dept.TotalSpend <= 0 {
		t.Error("current window should have spend")
	}
}

func TestExportCSV(t *testing.T) {
	summaries := []DepartmentPeriodSummary{
		{
			DepartmentName: "engineering",
			Periods: []PeriodSummary{
				{PeriodKey: "2026-08", Spend: 100.5, RequestCount: 10, InputTokens: 1000, OutputTokens: 500, TotalTokens: 1500},
				{PeriodKey: "2026-09", Spend: 200.75, RequestCount: 20, InputTokens: 2000, OutputTokens: 1000, TotalTokens: 3000},
			},
		},
		{
			DepartmentName: "finance",
			Periods: []PeriodSummary{
				{PeriodKey: "2026-08", Spend: 50.25, RequestCount: 5, InputTokens: 500, OutputTokens: 250, TotalTokens: 750},
			},
		},
	}

	data, err := ExportCSV(summaries)
	if err != nil {
		t.Fatalf("ExportCSV failed: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("expected non-empty CSV")
	}
	// Check header
	expected := "department,period,spend,request_count,input_tokens,output_tokens,total_tokens\n"
	if string(data[:len(expected)]) != expected {
		t.Errorf("unexpected header: got %q", string(data[:len(expected)]))
	}
}

func TestPeriodWindow(t *testing.T) {
	tests := []struct {
		period    Period
		time      time.Time
		wantStart int // day of month
		wantReset int // day of month for reset
	}{
		{PeriodMonthly, time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC), 1, 1},
		{PeriodMonthly, time.Date(2026, 12, 25, 0, 0, 0, 0, time.UTC), 1, 1},
		{PeriodQuarterly, time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC), 1, 1},
		{PeriodQuarterly, time.Date(2026, 5, 15, 0, 0, 0, 0, time.UTC), 1, 1},
	}

	for _, tt := range tests {
		t.Run(string(tt.period)+"-"+tt.time.Format("2006-01"), func(t *testing.T) {
			start, reset := PeriodWindow(tt.period, tt.time)
			if start.Day() != tt.wantStart {
				t.Errorf("start day = %d, want %d", start.Day(), tt.wantStart)
			}
			if reset.Day() != tt.wantReset {
				t.Errorf("reset day = %d, want %d", reset.Day(), tt.wantReset)
			}
			if !reset.After(start) {
				t.Error("reset must be after start")
			}
		})
	}
}

func TestPeriodKey(t *testing.T) {
	tests := []struct {
		period Period
		time   time.Time
		want   string
	}{
		{PeriodMonthly, time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC), "2026-08"},
		{PeriodMonthly, time.Date(2026, 12, 25, 0, 0, 0, 0, time.UTC), "2026-12"},
		{PeriodQuarterly, time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC), "2026-Q1"},
		{PeriodQuarterly, time.Date(2026, 5, 15, 0, 0, 0, 0, time.UTC), "2026-Q2"},
		{PeriodQuarterly, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), "2026-Q4"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := PeriodKey(tt.period, tt.time)
			if got != tt.want {
				t.Errorf("PeriodKey() = %q, want %q", got, tt.want)
			}
		})
	}
}

// compile-time check for usage.Plugin interface
var _ coreusage.Plugin = (*Tracker)(nil)
