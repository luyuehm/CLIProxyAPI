package management

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/alerts"
)

// GetAlerts returns the current alerts config, enabled channels, and recent
// events.
func (h *Handler) GetAlerts(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return
	}
	c.JSON(http.StatusOK, alerts.CurrentSnapshot())
}

// PutAlerts replaces the runtime alerts configuration and reinstalls the alert
// manager. The change applies immediately but is not persisted to disk.
func (h *Handler) PutAlerts(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return
	}

	var cfg alerts.Config
	if err := c.ShouldBindJSON(&cfg); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request", "message": err.Error()})
		return
	}

	h.cfg.Alerts = cfg
	alerts.Install(cfg)
	c.JSON(http.StatusOK, alerts.CurrentSnapshot())
}

type alertsTestRequest struct {
	Channel string `json:"channel"`
	Message string `json:"message"`
}

// PostAlertsTest sends a test message through one configured channel.
func (h *Handler) PostAlertsTest(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return
	}

	var request alertsTestRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request", "message": err.Error()})
		return
	}

	channel := alerts.ChannelKind(strings.TrimSpace(request.Channel))
	message := strings.TrimSpace(request.Message)
	if message == "" {
		message = "CLIProxyAPI alerts test message"
	}

	manager := alerts.Active()
	if manager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "alerts not installed"})
		return
	}
	if err := manager.SendTextTo(c.Request.Context(), channel, message); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "send_failed", "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"sent": channel, "message": message})
}
