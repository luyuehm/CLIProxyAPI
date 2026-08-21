package management

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/anomaly"
)

// GetAnomalyStats returns anomaly detection statistics for all tracked principals.
func (h *Handler) GetAnomalyStats(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}
	h.mu.Lock()
	cfg := h.cfg
	h.mu.Unlock()
	if cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "config not available"})
		return
	}

	// We need the engine reference from the server. This is set via a hook.
	// For now, the engine is accessible through the config reload hook context.
	engine := h.getAnomalyEngine()
	if engine == nil {
		c.JSON(http.StatusOK, gin.H{"stats": []anomaly.Stats{}, "enabled": false})
		return
	}

	stats := engine.AllStats(time.Now())
	c.JSON(http.StatusOK, gin.H{"stats": stats, "enabled": true})
}

// GetAnomalyEvents returns anomaly events, optionally filtered by principal.
func (h *Handler) GetAnomalyEvents(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}

	engine := h.getAnomalyEngine()
	if engine == nil {
		c.JSON(http.StatusOK, gin.H{"events": []anomaly.Event{}})
		return
	}

	principal := c.Query("principal")
	durationStr := c.Query("duration")
	var events []*anomaly.Event
	if durationStr != "" {
		duration, err := time.ParseDuration(durationStr)
		if err == nil {
			events = engine.RecentEvents(duration, time.Now())
		} else {
			events = engine.Events(principal)
		}
	} else {
		events = engine.Events(principal)
	}

	if events == nil {
		events = []*anomaly.Event{}
	}

	c.JSON(http.StatusOK, gin.H{"events": events})
}

// ResolveAnomalyEvent marks an anomaly event as resolved.
func (h *Handler) ResolveAnomalyEvent(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}

	var req struct {
		Principal string `json:"principal"`
		EventID   string `json:"event_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	engine := h.getAnomalyEngine()
	if engine == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "anomaly engine not available"})
		return
	}

	if engine.ResolveEvent(req.Principal, req.EventID, time.Now()) {
		c.JSON(http.StatusOK, gin.H{"status": "resolved"})
	} else {
		c.JSON(http.StatusNotFound, gin.H{"error": "event not found or already resolved"})
	}
}

// getAnomalyEngine retrieves the anomaly engine instance.
// This is a hook-based approach; the engine is stored as an interface
// accessible through the Handler's config reload hook context.
func (h *Handler) getAnomalyEngine() AnomalyEngine {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	engine := h.anomalyEngine
	h.mu.Unlock()
	return engine
}

// AnomalyEngine is the interface that the anomaly detection engine must implement.
type AnomalyEngine interface {
	Events(principal string) []*anomaly.Event
	RecentEvents(ago time.Duration, now time.Time) []*anomaly.Event
	AllStats(now time.Time) []*anomaly.Stats
	Stats(principal, provider string, now time.Time) *anomaly.Stats
	ResolveEvent(principal, eventID string, now time.Time) bool
}

// SetAnomalyEngine stores a reference to the anomaly engine for management API access.
func (h *Handler) SetAnomalyEngine(engine AnomalyEngine) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.anomalyEngine = engine
}
