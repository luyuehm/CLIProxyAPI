package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"cpa-usage-keeper/internal/entities"
	"cpa-usage-keeper/internal/repository"
	"cpa-usage-keeper/internal/service"
	"github.com/gin-gonic/gin"
)

type contentFilterRuleResponse struct {
	ID             int64    `json:"id"`
	Name           string   `json:"name"`
	Description    string   `json:"description"`
	Enabled        bool     `json:"enabled"`
	Scenario       string   `json:"scenario"`
	Action         string   `json:"action"`
	SensitiveWords []string `json:"sensitive_words"`
	PIITypes       []string `json:"pii_types"`
	WhiteList      []string `json:"white_list"`
	Models         []string `json:"models"`
	Priority       int      `json:"priority"`
	CreatedAt      string   `json:"created_at"`
	UpdatedAt      string   `json:"updated_at"`
}

type contentFilterRuleListResponse struct {
	Rules []contentFilterRuleResponse `json:"rules"`
}

type contentFilterLogListResponse struct {
	Logs  []entities.ContentFilterLog `json:"logs"`
	Total int64                       `json:"total"`
}

func toRuleResponse(r entities.ContentFilterRule) contentFilterRuleResponse {
	return contentFilterRuleResponse{
		ID:             r.ID,
		Name:           r.Name,
		Description:    r.Description,
		Enabled:        r.Enabled,
		Scenario:       r.Scenario,
		Action:         r.Action,
		SensitiveWords: splitStringList(r.SensitiveWords),
		PIITypes:       splitStringList(r.PIITypes),
		WhiteList:      splitStringList(r.WhiteList),
		Models:         splitStringList(r.Models),
		Priority:       r.Priority,
		CreatedAt:      r.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt:      r.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
}

func splitStringList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return []string{}
	}
	raw := strings.ReplaceAll(s, "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\r", "\n")
	raw = strings.ReplaceAll(raw, ",", "\n")
	parts := strings.Split(raw, "\n")
	var result []string
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

func registerContentFilterRoutes(router gin.IRoutes, provider service.ContentFilterProvider) {
	group := router

	// GET /contentfilter/rules
	group.GET("/contentfilter/rules", func(c *gin.Context) {
		if provider == nil {
			c.JSON(http.StatusOK, contentFilterRuleListResponse{Rules: []contentFilterRuleResponse{}})
			return
		}
		rules, err := provider.ListRules(c.Request.Context())
		if err != nil {
			writeInternalError(c, "list content filter rules failed", err)
			return
		}
		resp := make([]contentFilterRuleResponse, 0, len(rules))
		for _, r := range rules {
			resp = append(resp, toRuleResponse(r))
		}
		c.JSON(http.StatusOK, contentFilterRuleListResponse{Rules: resp})
	})

	// GET /contentfilter/rules/:id
	group.GET("/contentfilter/rules/:id", func(c *gin.Context) {
		if provider == nil {
			c.JSON(http.StatusNotImplemented, gin.H{"error": "content filter provider is not configured"})
			return
		}
		id, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil || id <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid rule id"})
			return
		}
		rule, err := provider.GetRule(c.Request.Context(), id)
		if err != nil {
			if errors.Is(err, repository.ErrContentFilterRuleNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": "rule not found"})
				return
			}
			writeInternalError(c, "get content filter rule failed", err)
			return
		}
		c.JSON(http.StatusOK, toRuleResponse(*rule))
	})

	// POST /contentfilter/rules
	group.POST("/contentfilter/rules", func(c *gin.Context) {
		if provider == nil {
			c.JSON(http.StatusNotImplemented, gin.H{"error": "content filter provider is not configured"})
			return
		}
		var req service.ContentFilterRuleCreateRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request: " + err.Error()})
			return
		}
		if strings.TrimSpace(req.Name) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "rule name is required"})
			return
		}
		rule, err := provider.CreateRule(c.Request.Context(), req)
		if err != nil {
			writeInternalError(c, "create content filter rule failed", err)
			return
		}
		c.JSON(http.StatusCreated, toRuleResponse(*rule))
	})

	// PUT /contentfilter/rules/:id
	group.PUT("/contentfilter/rules/:id", func(c *gin.Context) {
		if provider == nil {
			c.JSON(http.StatusNotImplemented, gin.H{"error": "content filter provider is not configured"})
			return
		}
		id, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil || id <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid rule id"})
			return
		}
		var req service.ContentFilterRuleUpdateRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request: " + err.Error()})
			return
		}
		rule, err := provider.UpdateRule(c.Request.Context(), id, req)
		if err != nil {
			if errors.Is(err, repository.ErrContentFilterRuleNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": "rule not found"})
				return
			}
			writeInternalError(c, "update content filter rule failed", err)
			return
		}
		c.JSON(http.StatusOK, toRuleResponse(*rule))
	})

	// DELETE /contentfilter/rules/:id
	group.DELETE("/contentfilter/rules/:id", func(c *gin.Context) {
		if provider == nil {
			c.JSON(http.StatusNotImplemented, gin.H{"error": "content filter provider is not configured"})
			return
		}
		id, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil || id <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid rule id"})
			return
		}
		if err := provider.DeleteRule(c.Request.Context(), id); err != nil {
			if errors.Is(err, repository.ErrContentFilterRuleNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": "rule not found"})
				return
			}
			writeInternalError(c, "delete content filter rule failed", err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "deleted", "id": id})
	})

	// GET /contentfilter/logs
	group.GET("/contentfilter/logs", func(c *gin.Context) {
		if provider == nil {
			c.JSON(http.StatusOK, contentFilterLogListResponse{Logs: []entities.ContentFilterLog{}, Total: 0})
			return
		}
		limit, _ := strconv.Atoi(c.Query("limit"))
		offset, _ := strconv.Atoi(c.Query("offset"))
		filterType := strings.TrimSpace(c.Query("filter_type"))
		action := strings.TrimSpace(c.Query("action"))
		model := strings.TrimSpace(c.Query("model"))

		var ruleID *int64
		if ruleIDStr := strings.TrimSpace(c.Query("rule_id")); ruleIDStr != "" {
			if rid, err := strconv.ParseInt(ruleIDStr, 10, 64); err == nil {
				ruleID = &rid
			}
		}

		logs, total, err := provider.ListLogs(c.Request.Context(), repository.ContentFilterLogQuery{
			RuleID:     ruleID,
			FilterType: filterType,
			Action:     action,
			Model:      model,
			Limit:      limit,
			Offset:     offset,
		})
		if err != nil {
			writeInternalError(c, "list content filter logs failed", err)
			return
		}
		c.JSON(http.StatusOK, contentFilterLogListResponse{
			Logs:  logs,
			Total: total,
		})
	})

	// POST /contentfilter/test
	group.POST("/contentfilter/test", func(c *gin.Context) {
		if provider == nil {
			c.JSON(http.StatusNotImplemented, gin.H{"error": "content filter provider is not configured"})
			return
		}
		var req service.FilterTextRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request: " + err.Error()})
			return
		}
		if req.ClientIP == "" {
			req.ClientIP = c.ClientIP()
		}
		resp, err := provider.FilterText(c.Request.Context(), req)
		if err != nil {
			writeInternalError(c, "filter text test failed", err)
			return
		}
		c.JSON(http.StatusOK, resp)
	})
}
