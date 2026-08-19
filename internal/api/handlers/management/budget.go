package management

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/costbudget"
)

// GetBudget returns the current cost budget report snapshot. When budgeting is
// disabled or no tracker is installed, it returns an enabled=false report so
// callers can distinguish "feature off" from "no spend yet".
func (h *Handler) GetBudget(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return
	}
	c.JSON(http.StatusOK, costbudget.ReportSnapshot())
}
