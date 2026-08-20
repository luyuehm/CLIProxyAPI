package management

import (
	"mime"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/costallocation"
)

// GetCostAllocation returns the current department cost allocation report
// snapshot. When allocation is disabled or no tracker is installed, it returns
// an enabled=false report so callers can distinguish "feature off" from
// "no spend yet".
func (h *Handler) GetCostAllocation(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return
	}
	c.JSON(http.StatusOK, costallocation.ReportSnapshot())
}

// ExportCostAllocation returns a CSV export of the department cost allocation
// summary with historical period buckets.
func (h *Handler) ExportCostAllocation(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return
	}

	summaries, err := costallocation.PeriodSummarySnapshot()
	if err != nil || len(summaries) == 0 {
		empty, csvErr := costallocation.ExportCSV(nil)
		if csvErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "export failed"})
			return
		}
		writeCSV(c, empty, "cost-allocation")
		return
	}

	data, err := costallocation.ExportCSV(summaries)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "export failed"})
		return
	}
	writeCSV(c, data, "cost-allocation")
}

func writeCSV(c *gin.Context, data []byte, baseName string) {
	if c == nil {
		return
	}
	now := time.Now().UTC().Format("20060102")
	filename := baseName + "-" + now + ".csv"
	c.Header("Content-Disposition", "attachment; filename=\""+filename+"\"")
	c.Data(http.StatusOK, mimeTypeForCSV(), data)
}

func mimeTypeForCSV() string {
	mt := mime.TypeByExtension(".csv")
	if mt == "" {
		return "text/csv; charset=utf-8"
	}
	return mt
}
