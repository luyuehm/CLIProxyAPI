package costallocation

import (
	"context"
	"strings"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestTrackerAggregationAndReporting(t *testing.T) {
	cfg := AllocationConfig{
		Enabled:           true,
		Currency:          "USD",
		DefaultDepartment: "Unallocated",
		DefaultTeam:       "Default",
		DefaultProject:    "Default",
		Prices: []PriceRate{
			{
				Provider:        "openai",
				Model:           "gpt-4o",
				InputRatePer1K:  0.01,
				OutputRatePer1K: 0.03,
			},
			{
				Provider:        "anthropic",
				Model:           "claude-3-5-sonnet",
				InputRatePer1K:  0.003,
				OutputRatePer1K: 0.015,
			},
		},
		Rules: []AllocationRule{
			{
				Department:     "Engineering",
				Team:           "Backend",
				Project:        "API-Server",
				APIKeyPrefixes: []string{"sk-backend-"},
			},
			{
				Department:     "Product",
				Team:           "Design",
				Project:        "Prototype",
				APIKeyPrefixes: []string{"sk-product-"},
			},
		},
	}

	tracker := NewTracker(cfg)
	if !tracker.Enabled() {
		t.Fatal("expected tracker to be enabled")
	}

	ctx := context.Background()
	reqTime1 := time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC)
	reqTime2 := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)

	// Record 1: Engineering Backend API-Server
	tracker.HandleUsage(ctx, coreusage.Record{
		Provider:    "openai",
		Model:       "gpt-4o",
		APIKey:      "sk-backend-001",
		RequestedAt: reqTime1,
		Detail: coreusage.Detail{
			TokenBreakdown: coreusage.TokenBreakdown{
				SchemaVersion: coreusage.TokenAccountingSchemaVersion,
				Quality:       coreusage.TokenAccountingQualityComplete,
				Input: coreusage.TokenInputBreakdown{
					TotalTokens:    1000,
					UncachedTokens: 1000,
				},
				Output: coreusage.TokenOutputBreakdown{
					TotalTokens:        2000,
					NonReasoningTokens: 2000,
				},
				TotalTokens: 3000,
			},
		},
	})

	// Record 2: Product Design Prototype
	tracker.HandleUsage(ctx, coreusage.Record{
		Provider:    "anthropic",
		Model:       "claude-3-5-sonnet",
		APIKey:      "sk-product-002",
		RequestedAt: reqTime2,
		Detail: coreusage.Detail{
			TokenBreakdown: coreusage.TokenBreakdown{
				SchemaVersion: coreusage.TokenAccountingSchemaVersion,
				Quality:       coreusage.TokenAccountingQualityComplete,
				Input: coreusage.TokenInputBreakdown{
					TotalTokens:    2000,
					UncachedTokens: 2000,
				},
				Output: coreusage.TokenOutputBreakdown{
					TotalTokens:        1000,
					NonReasoningTokens: 1000,
				},
				TotalTokens: 3000,
			},
		},
	})

	// Record 3: Failed request
	tracker.HandleUsage(ctx, coreusage.Record{
		Provider:    "openai",
		Model:       "gpt-4o",
		APIKey:      "sk-backend-001",
		RequestedAt: reqTime2,
		Failed:      true,
	})

	report := tracker.Report()
	if report.Total.Requests != 3 {
		t.Fatalf("expected 3 total requests, got %d", report.Total.Requests)
	}
	if report.Total.FailedRequests != 1 {
		t.Fatalf("expected 1 failed request, got %d", report.Total.FailedRequests)
	}

	// Cost 1: (1000/1000)*0.01 + (2000/1000)*0.03 = 0.01 + 0.06 = 0.07
	// Cost 2: (2000/1000)*0.003 + (1000/1000)*0.015 = 0.006 + 0.015 = 0.021
	// Total Cost = 0.091
	expectedTotalCost := 0.091
	if report.Total.Cost < expectedTotalCost-1e-5 || report.Total.Cost > expectedTotalCost+1e-5 {
		t.Fatalf("expected total cost %f, got %f", expectedTotalCost, report.Total.Cost)
	}

	// Check Department breakdown
	engDept, ok := report.Departments["Engineering"]
	if !ok || engDept.Requests != 2 {
		t.Fatalf("expected 2 requests for Engineering, got %+v", engDept)
	}
	prodDept, ok := report.Departments["Product"]
	if !ok || prodDept.Requests != 1 {
		t.Fatalf("expected 1 request for Product, got %+v", prodDept)
	}

	// Check Monthly period aggregation
	mStat, ok := report.Monthly["2026-08"]
	if !ok || mStat.Requests != 3 {
		t.Fatalf("expected monthly 2026-08 with 3 requests, got %+v", mStat)
	}

	// Check Quarterly period aggregation
	qStat, ok := report.Quarterly["2026-Q3"]
	if !ok || qStat.Requests != 3 {
		t.Fatalf("expected quarterly 2026-Q3 with 3 requests, got %+v", qStat)
	}

	// Check Summary rankings
	summary := tracker.Summary()
	if len(summary.DepartmentRankings) != 2 {
		t.Fatalf("expected 2 department rankings, got %d", len(summary.DepartmentRankings))
	}
	if summary.DepartmentRankings[0].Department != "Engineering" {
		t.Fatalf("expected Engineering to be ranked #1, got %s", summary.DepartmentRankings[0].Department)
	}

	// Check CSV Export
	csvBytes, err := tracker.ExportCSV(QueryOptions{PeriodType: PeriodMonthly})
	if err != nil {
		t.Fatalf("ExportCSV failed: %v", err)
	}
	csvStr := string(csvBytes)
	if !strings.Contains(csvStr, "Engineering") || !strings.Contains(csvStr, "Product") {
		t.Fatalf("expected CSV to contain departments, got:\n%s", csvStr)
	}
	if !strings.Contains(csvStr, "2026-08") {
		t.Fatalf("expected CSV to contain period 2026-08, got:\n%s", csvStr)
	}
}
