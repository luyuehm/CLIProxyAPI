package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"cpa-usage-keeper/internal/alert"
	"github.com/gin-gonic/gin"
)

type alertHandler struct {
	svc *alert.Service
}

func NewAlertHandler(svc *alert.Service) *alertHandler {
	return &alertHandler{svc: svc}
}

func (h *alertHandler) registerRoutes(router gin.IRoutes) {
	// 告警通道
	router.GET("/alerts/channels", h.listChannels)
	router.POST("/alerts/channels", h.createChannel)
	router.PUT("/alerts/channels/:id", h.updateChannel)
	router.DELETE("/alerts/channels/:id", h.deleteChannel)

	// 告警规则
	router.GET("/alerts/rules", h.listRules)
	router.POST("/alerts/rules", h.createRule)
	router.PUT("/alerts/rules/:id", h.updateRule)
	router.DELETE("/alerts/rules/:id", h.deleteRule)

	// 告警事件
	router.GET("/alerts/events", h.listEvents)
	router.POST("/alerts/events/:id/retry", h.retryEvent)
}

func (h *alertHandler) listChannels(c *gin.Context) {
	channels, err := h.svc.ListChannels()
	if err != nil {
		writeInternalError(c, "list alert channels failed", err)
		return
	}
	c.JSON(http.StatusOK, channels)
}

func (h *alertHandler) createChannel(c *gin.Context) {
	var req alert.ChannelCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request: " + err.Error()})
		return
	}
	if !alert.IsValidPlatform(req.Platform) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid platform, must be one of: feishu, dingtalk, wecom"})
		return
	}
	channel, err := h.svc.CreateChannel(req)
	if err != nil {
		writeInternalError(c, "create alert channel failed", err)
		return
	}
	c.JSON(http.StatusCreated, channel)
}

func (h *alertHandler) updateChannel(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid channel id"})
		return
	}
	var req alert.ChannelUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request: " + err.Error()})
		return
	}
	if req.Platform != nil && !alert.IsValidPlatform(*req.Platform) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid platform, must be one of: feishu, dingtalk, wecom"})
		return
	}
	channel, err := h.svc.UpdateChannel(id, req)
	if err != nil {
		if errors.Is(err, alert.ErrChannelNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "alert channel not found"})
			return
		}
		writeInternalError(c, "update alert channel failed", err)
		return
	}
	c.JSON(http.StatusOK, channel)
}

func (h *alertHandler) deleteChannel(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid channel id"})
		return
	}
	if err := h.svc.DeleteChannel(id); err != nil {
		if errors.Is(err, alert.ErrChannelNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "alert channel not found"})
			return
		}
		writeInternalError(c, "delete alert channel failed", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "deleted"})
}

func (h *alertHandler) listRules(c *gin.Context) {
	rules, err := h.svc.ListRules()
	if err != nil {
		writeInternalError(c, "list alert rules failed", err)
		return
	}
	c.JSON(http.StatusOK, rules)
}

func (h *alertHandler) createRule(c *gin.Context) {
	var req alert.RuleCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request: " + err.Error()})
		return
	}
	if !alert.IsValidMetricType(req.MetricType) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid metric_type, must be one of: usage_threshold, quota_exhausted, error_rate"})
		return
	}
	if !alert.IsValidOperator(req.ConditionOp) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid condition_op, must be one of: gt, gte, lt, lte"})
		return
	}
	rule, err := h.svc.CreateRule(req)
	if err != nil {
		if strings.Contains(err.Error(), "alert channel not found") {
			c.JSON(http.StatusBadRequest, gin.H{"error": "referenced alert channel not found"})
			return
		}
		writeInternalError(c, "create alert rule failed", err)
		return
	}
	c.JSON(http.StatusCreated, rule)
}

func (h *alertHandler) updateRule(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid rule id"})
		return
	}
	var req alert.RuleUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request: " + err.Error()})
		return
	}
	if req.MetricType != nil && !alert.IsValidMetricType(*req.MetricType) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid metric_type, must be one of: usage_threshold, quota_exhausted, error_rate"})
		return
	}
	if req.ConditionOp != nil && !alert.IsValidOperator(*req.ConditionOp) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid condition_op, must be one of: gt, gte, lt, lte"})
		return
	}
	rule, err := h.svc.UpdateRule(id, req)
	if err != nil {
		if errors.Is(err, alert.ErrRuleNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "alert rule not found"})
			return
		}
		writeInternalError(c, "update alert rule failed", err)
		return
	}
	c.JSON(http.StatusOK, rule)
}

func (h *alertHandler) deleteRule(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid rule id"})
		return
	}
	if err := h.svc.DeleteRule(id); err != nil {
		if errors.Is(err, alert.ErrRuleNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "alert rule not found"})
			return
		}
		writeInternalError(c, "delete alert rule failed", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "deleted"})
}

func (h *alertHandler) listEvents(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	events, err := h.svc.ListEvents(limit)
	if err != nil {
		writeInternalError(c, "list alert events failed", err)
		return
	}
	c.JSON(http.StatusOK, events)
}

func (h *alertHandler) retryEvent(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid event id"})
		return
	}
	event, sendErr := h.svc.RetryEvent(c.Request.Context(), id)
	if sendErr != nil {
		c.JSON(http.StatusOK, gin.H{"event": event, "retry_error": sendErr.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"event": event})
}