package management

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/costallocation"
)

// GetCostAllocationReport returns the current cost allocation report snapshot.
func (h *Handler) GetCostAllocationReport(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return
	}

	report := costallocation.ReportSnapshot()
	c.JSON(http.StatusOK, report)
}

// GetCostAllocationSummary returns high-level summary and rankings across departments and projects.
func (h *Handler) GetCostAllocationSummary(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return
	}

	summary := costallocation.SummarySnapshot()
	c.JSON(http.StatusOK, summary)
}

// GetCostAllocationDepartments returns the department-level cost breakdown list.
func (h *Handler) GetCostAllocationDepartments(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return
	}

	depts := costallocation.DepartmentsSnapshot()
	c.JSON(http.StatusOK, gin.H{
		"departments": depts,
	})
}

// ExportCostAllocation exports cost allocation reports as CSV or JSON.
func (h *Handler) ExportCostAllocation(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return
	}

	format := strings.ToLower(strings.TrimSpace(c.DefaultQuery("format", "csv")))
	opts := costallocation.QueryOptions{
		PeriodType: costallocation.Period(c.Query("period_type")),
		PeriodKey:  c.Query("period_key"),
		Department: c.Query("department"),
		Team:       c.Query("team"),
		Project:    c.Query("project"),
		APIKey:     c.Query("api_key"),
		Provider:   c.Query("provider"),
		Model:      c.Query("model"),
	}

	if format == "json" {
		c.JSON(http.StatusOK, costallocation.ReportSnapshot())
		return
	}

	csvBytes, err := costallocation.ExportSnapshotCSV(opts)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to generate CSV: %v", err)})
		return
	}

	fileName := fmt.Sprintf("cost_allocation_%s.csv", time.Now().Format("20060102_150405"))
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", fileName))
	c.Data(http.StatusOK, "text/csv; charset=utf-8", csvBytes)
}
