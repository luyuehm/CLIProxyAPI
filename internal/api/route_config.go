package api

import (
	"net/http"
	"strings"

	"cpa-usage-keeper/internal/repository/dto"
	"cpa-usage-keeper/internal/service"
	"github.com/gin-gonic/gin"
)

type routeConfigEntryResponse struct {
	ID          int64  `json:"id"`
	Model       string `json:"model"`
	Enabled     bool   `json:"enabled"`
	Strategy    string `json:"strategy"`
	BaseURL     string `json:"base_url"`
	APIKey      string `json:"api_key,omitempty"`
	Weight      int    `json:"weight"`
	Description string `json:"description,omitempty"`
}

type routeConfigListResponse struct {
	Routes []routeConfigEntryResponse `json:"routes"`
}

type upsertRouteConfigRequest struct {
	Model       string `json:"model"`
	Enabled     *bool  `json:"enabled,omitempty"`
	Strategy    string `json:"strategy,omitempty"`
	BaseURL     string `json:"base_url"`
	APIKey      string `json:"api_key,omitempty"`
	Weight      *int   `json:"weight,omitempty"`
	Description string `json:"description,omitempty"`
}

func registerRouteConfigRoutes(router gin.IRoutes, routeProvider service.RouteConfigProvider) {
	router.GET("/routes", func(c *gin.Context) {
		if routeProvider == nil {
			c.JSON(http.StatusOK, routeConfigListResponse{Routes: []routeConfigEntryResponse{}})
			return
		}
		routes, err := routeProvider.ListRoutes(c.Request.Context())
		if err != nil {
			writeInternalError(c, "list routes failed", err)
			return
		}
		response := make([]routeConfigEntryResponse, 0, len(routes))
		for _, r := range routes {
			response = append(response, toRouteEntryResponse(r))
		}
		c.JSON(http.StatusOK, routeConfigListResponse{Routes: response})
	})

	router.GET("/routes/:model", func(c *gin.Context) {
		if routeProvider == nil {
			c.JSON(http.StatusNotImplemented, gin.H{"error": "route config provider is not configured"})
			return
		}
		model := strings.TrimSpace(c.Param("model"))
		if model == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "model is required"})
			return
		}
		route, err := routeProvider.GetRoute(c.Request.Context(), model)
		if err != nil {
			writeInternalError(c, "get route failed", err)
			return
		}
		if route == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "route not found"})
			return
		}
		c.JSON(http.StatusOK, toRouteEntryResponse(*route))
	})

	router.PUT("/routes", func(c *gin.Context) {
		upsertRoute(c, routeProvider, "")
	})

	router.PUT("/routes/:model", func(c *gin.Context) {
		upsertRoute(c, routeProvider, c.Param("model"))
	})

	router.DELETE("/routes", func(c *gin.Context) {
		if routeProvider == nil {
			c.JSON(http.StatusNotImplemented, gin.H{"error": "route config provider is not configured"})
			return
		}
		model := strings.TrimSpace(c.Query("model"))
		if model == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "model is required"})
			return
		}
		if err := routeProvider.DeleteRoute(c.Request.Context(), model); err != nil {
			if strings.Contains(err.Error(), "required") {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			writeInternalError(c, "delete route failed", err)
			return
		}
		c.Status(http.StatusNoContent)
	})
}

func upsertRoute(c *gin.Context, routeProvider service.RouteConfigProvider, pathModel string) {
	if routeProvider == nil {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "route config provider is not configured"})
		return
	}
	var request upsertRouteConfigRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	model := strings.TrimSpace(pathModel)
	if model == "" {
		model = strings.TrimSpace(request.Model)
	}
	if model == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model is required"})
		return
	}

	enabled := true
	if request.Enabled != nil {
		enabled = *request.Enabled
	}
	weight := 100
	if request.Weight != nil {
		weight = *request.Weight
	}

	route, err := routeProvider.UpsertRoute(c.Request.Context(), dto.RouteConfigInput{
		Model:       model,
		Enabled:     enabled,
		Strategy:    request.Strategy,
		BaseURL:     request.BaseURL,
		APIKey:      request.APIKey,
		Weight:      weight,
		Description: request.Description,
	})
	if err != nil {
		if strings.Contains(err.Error(), "required") {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		writeInternalError(c, "upsert route failed", err)
		return
	}
	c.JSON(http.StatusOK, toRouteEntryResponse(*route))
}

func toRouteEntryResponse(r dto.RouteConfigEntry) routeConfigEntryResponse {
	return routeConfigEntryResponse{
		ID:          r.ID,
		Model:       r.Model,
		Enabled:     r.Enabled,
		Strategy:    r.Strategy,
		BaseURL:     r.BaseURL,
		APIKey:      r.APIKey,
		Weight:      r.Weight,
		Description: r.Description,
	}
}