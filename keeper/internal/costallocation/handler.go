package costallocation

import (
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"cpa-usage-keeper/internal/entities"
	"github.com/gin-gonic/gin"
)

// Handler 是费用分摊模块的 HTTP 处理入口。
type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// RegisterRoutes 注册费用分摊 API 路由（挂在 admin 保护组下）。
// - GET  /costallocation/departments — 部门费用列表
// - GET  /costallocation/rules — 分摊规则列表
// - POST /costallocation/rules — 新建分摊规则
// - PUT  /costallocation/rules/:id — 更新分摊规则
// - DELETE /costallocation/rules/:id — 删除分摊规则
// - GET  /costallocation/report — 费用报表
// - GET  /costallocation/export.csv — 导出 CSV
func (h *Handler) RegisterRoutes(router gin.IRoutes) {
	router.GET("/costallocation/departments", h.listDepartments)
	router.GET("/costallocation/rules", h.listRules)
	router.POST("/costallocation/rules", h.createRule)
	router.PUT("/costallocation/rules/:id", h.updateRule)
	router.DELETE("/costallocation/rules/:id", h.deleteRule)
	router.GET("/costallocation/report", h.getReport)
	router.GET("/costallocation/export.csv", h.exportCSV)
}

func (h *Handler) listDepartments(c *gin.Context) {
	if !h.ready(c) {
		return
	}
	from, to, dimension, ok := h.parseRangeDimension(c)
	if !ok {
		return
	}
	response, err := h.service.ListDepartments(from, to, dimension)
	if err != nil {
		h.writeError(c, "department cost lookup failed", err)
		return
	}
	c.JSON(http.StatusOK, response)
}

func (h *Handler) listRules(c *gin.Context) {
	if !h.ready(c) {
		return
	}
	rules, err := h.service.ListRules()
	if err != nil {
		h.writeError(c, "cost allocation rules lookup failed", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"rules": rules})
}

type createRuleRequest struct {
	Name        string   `json:"name"`
	Dimension   string   `json:"dimension"`
	MatchType   string   `json:"match_type"`
	MatchValues []string `json:"match_values"`
	Enabled     *bool    `json:"enabled,omitempty"`
	Priority    *int     `json:"priority,omitempty"`
	Note        string   `json:"note,omitempty"`
}

func (h *Handler) createRule(c *gin.Context) {
	if !h.ready(c) {
		return
	}
	var request createRuleRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	enabled := true
	if request.Enabled != nil {
		enabled = *request.Enabled
	}
	priority := 0
	if request.Priority != nil {
		priority = *request.Priority
	}
	view, err := h.service.CreateRule(CostAllocationRuleCreateInput{
		Name:        request.Name,
		Dimension:   entities.CostAllocationDimension(strings.TrimSpace(request.Dimension)),
		MatchType:   entities.CostAllocationMatchType(strings.TrimSpace(request.MatchType)),
		MatchValues: request.MatchValues,
		Enabled:     enabled,
		Priority:    priority,
		Note:        strings.TrimSpace(request.Note),
	})
	if err != nil {
		h.writeError(c, "cost allocation rule create failed", err)
		return
	}
	c.JSON(http.StatusCreated, view)
}

func (h *Handler) updateRule(c *gin.Context) {
	if !h.ready(c) {
		return
	}
	id, ok := ruleIDFromParam(c)
	if !ok {
		return
	}
	var request struct {
		Name        *string   `json:"name"`
		Dimension   *string   `json:"dimension"`
		MatchType   *string   `json:"match_type"`
		MatchValues *[]string `json:"match_values"`
		Enabled     *bool     `json:"enabled"`
		Priority    *int      `json:"priority"`
		Note        *string   `json:"note"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	input := CostAllocationRuleUpdateInput{}
	if request.Name != nil {
		input.Name = request.Name
	}
	if request.Dimension != nil {
		dimension := entities.CostAllocationDimension(strings.TrimSpace(*request.Dimension))
		input.Dimension = &dimension
	}
	if request.MatchType != nil {
		matchType := entities.CostAllocationMatchType(strings.TrimSpace(*request.MatchType))
		input.MatchType = &matchType
	}
	if request.MatchValues != nil {
		input.MatchValues = request.MatchValues
	}
	if request.Enabled != nil {
		input.Enabled = request.Enabled
	}
	if request.Priority != nil {
		input.Priority = request.Priority
	}
	if request.Note != nil {
		input.Note = request.Note
	}

	view, err := h.service.UpdateRule(id, input)
	if err != nil {
		h.writeError(c, "cost allocation rule update failed", err)
		return
	}
	c.JSON(http.StatusOK, view)
}

func (h *Handler) deleteRule(c *gin.Context) {
	if !h.ready(c) {
		return
	}
	id, ok := ruleIDFromParam(c)
	if !ok {
		return
	}
	if err := h.service.DeleteRule(id); err != nil {
		if err == ErrRuleNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "cost allocation rule not found"})
			return
		}
		h.writeError(c, "cost allocation rule delete failed", err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) getReport(c *gin.Context) {
	if !h.ready(c) {
		return
	}
	from, to, dimension, ok := h.parseRangeDimension(c)
	if !ok {
		return
	}
	report, err := h.service.Report(from, to, dimension)
	if err != nil {
		h.writeError(c, "cost allocation report lookup failed", err)
		return
	}
	c.JSON(http.StatusOK, report)
}

func (h *Handler) exportCSV(c *gin.Context) {
	if !h.ready(c) {
		return
	}
	from, to, dimension, ok := h.parseRangeDimension(c)
	if !ok {
		return
	}
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", `attachment; filename="cost-allocation.csv"`)
	if err := h.service.ExportCSV(c.Writer, from, to, dimension); err != nil {
		// CSV 已开始写入时不能改状态码；记录并结束响应。
		slog.Error("cost allocation csv export failed", "error", err)
		return
	}
}

// ready 检查 service 是否配置；未配置时写 500 并返回 false。
func (h *Handler) ready(c *gin.Context) bool {
	if h.service == nil {
		h.writeError(c, "cost allocation service is not configured", nil)
		return false
	}
	return true
}

// parseRangeDimension 解析 from/to/dimension 查询参数，失败时已写响应并返回 false。
func (h *Handler) parseRangeDimension(c *gin.Context) (time.Time, time.Time, entities.CostAllocationDimension, bool) {
	from, err := parseTimeQuery(c, "from")
	if err != nil {
		h.writeTimeError(c, "from", err)
		return time.Time{}, time.Time{}, "", false
	}
	to, err := parseTimeQuery(c, "to")
	if err != nil {
		h.writeTimeError(c, "to", err)
		return time.Time{}, time.Time{}, "", false
	}
	dimension := entities.CostAllocationDimension(strings.TrimSpace(c.Query("dimension")))
	if dimension == "" {
		dimension = entities.CostAllocationDimensionDepartment
	}
	return from, to, dimension, true
}

func parseTimeQuery(c *gin.Context, key string) (time.Time, error) {
	raw := strings.TrimSpace(c.Query(key))
	if raw == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid %s: %q (use RFC3339)", key, raw)
	}
	return parsed, nil
}

func (h *Handler) writeTimeError(c *gin.Context, key string, err error) {
	c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
}

func ruleIDFromParam(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid rule id"})
		return 0, false
	}
	return id, true
}

func (h *Handler) writeError(c *gin.Context, message string, err error) {
	if err != nil {
		if strings.Contains(err.Error(), "unsupported dimension") ||
			strings.Contains(err.Error(), "is required") ||
			strings.Contains(err.Error(), "match type") {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err == ErrRuleNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": ErrRuleNotFound.Error()})
			return
		}
		slog.Error(message, "error", err)
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": "internal server error"})
}
