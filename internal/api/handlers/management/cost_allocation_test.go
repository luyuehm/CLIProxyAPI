package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/costallocation"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestCostAllocationHandlers(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tracker := costallocation.NewTracker(costallocation.AllocationConfig{
		Enabled:  true,
		Currency: "USD",
		Prices: []costallocation.PriceRate{
			{
				Provider:        "openai",
				Model:           "gpt-4o",
				InputRatePer1K:  0.01,
				OutputRatePer1K: 0.03,
			},
		},
		Rules: []costallocation.AllocationRule{
			{
				Department:     "Engineering",
				Team:           "Backend",
				Project:        "API-Server",
				APIKeyPrefixes: []string{"sk-backend-"},
			},
		},
	})
	costallocation.Install(tracker)

	// Ingest sample usage
	tracker.HandleUsage(t.Context(), coreusage.Record{
		Provider:    "openai",
		Model:       "gpt-4o",
		APIKey:      "sk-backend-123",
		RequestedAt: time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC),
		Detail: coreusage.Detail{
			TokenBreakdown: coreusage.TokenBreakdown{
				SchemaVersion: coreusage.TokenAccountingSchemaVersion,
				Quality:       coreusage.TokenAccountingQualityComplete,
				Input: coreusage.TokenInputBreakdown{
					TotalTokens:    1000,
					UncachedTokens: 1000,
				},
				Output: coreusage.TokenOutputBreakdown{
					TotalTokens:        1000,
					NonReasoningTokens: 1000,
				},
				TotalTokens: 2000,
			},
		},
	})

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)

	// 1. Test GetCostAllocationReport
	{
		rec := httptest.NewRecorder()
		ginCtx, _ := gin.CreateTestContext(rec)
		ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/cost-allocation/report", nil)
		h.GetCostAllocationReport(ginCtx)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var rep costallocation.AllocationReport
		if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
			t.Fatalf("unmarshal report: %v", err)
		}
		if rep.Total.Requests != 1 {
			t.Fatalf("expected 1 total request, got %d", rep.Total.Requests)
		}
		if _, ok := rep.Departments["Engineering"]; !ok {
			t.Fatal("expected Engineering department in report")
		}
	}

	// 2. Test GetCostAllocationSummary
	{
		rec := httptest.NewRecorder()
		ginCtx, _ := gin.CreateTestContext(rec)
		ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/cost-allocation/summary", nil)
		h.GetCostAllocationSummary(ginCtx)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		var sum costallocation.SummaryReport
		if err := json.Unmarshal(rec.Body.Bytes(), &sum); err != nil {
			t.Fatalf("unmarshal summary: %v", err)
		}
		if len(sum.DepartmentRankings) != 1 || sum.DepartmentRankings[0].Department != "Engineering" {
			t.Fatalf("unexpected summary rankings: %+v", sum.DepartmentRankings)
		}
	}

	// 3. Test GetCostAllocationDepartments
	{
		rec := httptest.NewRecorder()
		ginCtx, _ := gin.CreateTestContext(rec)
		ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/cost-allocation/departments", nil)
		h.GetCostAllocationDepartments(ginCtx)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		var resp struct {
			Departments []costallocation.DepartmentSummaryItem `json:"departments"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal departments: %v", err)
		}
		if len(resp.Departments) != 1 {
			t.Fatalf("expected 1 department, got %d", len(resp.Departments))
		}
	}

	// 4. Test ExportCostAllocation CSV
	{
		rec := httptest.NewRecorder()
		ginCtx, _ := gin.CreateTestContext(rec)
		ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/cost-allocation/export?format=csv", nil)
		h.ExportCostAllocation(ginCtx)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		contentType := rec.Header().Get("Content-Type")
		if !strings.Contains(contentType, "text/csv") {
			t.Fatalf("expected text/csv content-type, got %s", contentType)
		}
		bodyStr := rec.Body.String()
		if !strings.Contains(bodyStr, "Engineering") || !strings.Contains(bodyStr, "Backend") {
			t.Fatalf("expected CSV content to contain Engineering, got:\n%s", bodyStr)
		}
	}

	// 5. Test ExportCostAllocation JSON
	{
		rec := httptest.NewRecorder()
		ginCtx, _ := gin.CreateTestContext(rec)
		ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/cost-allocation/export?format=json", nil)
		h.ExportCostAllocation(ginCtx)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		var rep costallocation.AllocationReport
		if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
			t.Fatalf("unmarshal json export: %v", err)
		}
		if rep.Total.Requests != 1 {
			t.Fatalf("expected 1 request, got %d", rep.Total.Requests)
		}
	}
}
