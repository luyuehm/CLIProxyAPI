package costbudget

import (
	"log/slog"
	"net/http"
	"strings"

	"cpa-usage-keeper/internal/entities"
	"github.com/gin-gonic/gin"
)

// Handler 是预算模块的 HTTP 处理入口。
type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// RegisterRoutes 注册预算 API 路由（挂在 admin 保护组下）。
func (h *Handler) RegisterRoutes(router gin.IRoutes) {
	router.GET("/budget/config", h.getConfig)
	router.GET("/budget", h.getConfig)
	router.PUT("/budget", h.updateConfig)
	router.GET("/budget/usage", h.getUsage)
	router.GET("/budget/report", h.getReport)
}

type updateBudgetRequest struct {
	Period         string  `json:"period"`
	Amount         float64 `json:"amount"`
	AlertThreshold *float64 `json:"alert_threshold,omitempty"`
	AlertEnabled   *bool   `json:"alert_enabled,omitempty"`
}

func (h *Handler) getConfig(c *gin.Context) {
	if h.service == nil {
		writeBudgetInternalError(c, "budget service is not configured", nil)
		return
	}
	period, ok := budgetPeriodFromQuery(c)
	if !ok {
		return
	}
	config, err := h.service.GetConfig(period)
	if err != nil {
		if strings.Contains(err.Error(), "invalid budget period") {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		writeBudgetInternalError(c, "budget config lookup failed", err)
		return
	}
	c.JSON(http.StatusOK, config)
}

func (h *Handler) updateConfig(c *gin.Context) {
	if h.service == nil {
		writeBudgetInternalError(c, "budget service is not configured", nil)
		return
	}
	var request updateBudgetRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	period := entities.BudgetPeriod(strings.TrimSpace(request.Period))
	if period == "" {
		period = entities.BudgetPeriodMonthly
	}
	threshold := defaultAlertThreshold
	if request.AlertThreshold != nil {
		threshold = *request.AlertThreshold
	}
	enabled := true
	if request.AlertEnabled != nil {
		enabled = *request.AlertEnabled
	}
	config, err := h.service.UpdateConfig(BudgetUpdateInput{
		Period:         period,
		Amount:         request.Amount,
		AlertThreshold: threshold,
		AlertEnabled:   enabled,
	})
	if err != nil {
		if strings.Contains(err.Error(), "invalid budget period") ||
			strings.Contains(err.Error(), "must be") {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		writeBudgetInternalError(c, "budget config update failed", err)
		return
	}
	c.JSON(http.StatusOK, config)
}

func (h *Handler) getUsage(c *gin.Context) {
	if h.service == nil {
		writeBudgetInternalError(c, "budget service is not configured", nil)
		return
	}
	period, ok := budgetPeriodFromQuery(c)
	if !ok {
		return
	}
	usage, err := h.service.Usage(period)
	if err != nil {
		if strings.Contains(err.Error(), "invalid budget period") {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		writeBudgetInternalError(c, "budget usage lookup failed", err)
		return
	}
	c.JSON(http.StatusOK, usage)
}

func (h *Handler) getReport(c *gin.Context) {
	if h.service == nil {
		writeBudgetInternalError(c, "budget service is not configured", nil)
		return
	}
	period, ok := budgetPeriodFromQuery(c)
	if !ok {
		return
	}
	report, err := h.service.Report(period)
	if err != nil {
		if strings.Contains(err.Error(), "invalid budget period") {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		writeBudgetInternalError(c, "budget report lookup failed", err)
		return
	}
	c.JSON(http.StatusOK, report)
}

func budgetPeriodFromQuery(c *gin.Context) (entities.BudgetPeriod, bool) {
	raw := strings.TrimSpace(c.Query("period"))
	if raw == "" {
		return entities.BudgetPeriodMonthly, true
	}
	switch entities.BudgetPeriod(raw) {
	case entities.BudgetPeriodMonthly, entities.BudgetPeriodQuarterly, entities.BudgetPeriodYearly:
		return entities.BudgetPeriod(raw), true
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported budget period, use monthly, quarterly or yearly"})
		return "", false
	}
}

func writeBudgetInternalError(c *gin.Context, message string, err error) {
	if err != nil {
		slog.Error(message, "error", err)
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": "internal server error"})
}
