package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/contentfilter"
	"github.com/tidwall/gjson"
)

// contentFilterCache holds the most recently compiled content filter per SDK
// config pointer. Filters are cheap to build but rebuilding per request adds
// avoidable regex compilation under load; the cache keeps the hot path to a
// pointer compare and a single map lookup.
var contentFilterCache = newContentFilterCache()

// ApplyContentFilter runs the configured request content filter against the
// raw request body. It returns the (possibly mutated) body and, when the
// filter blocks the request, writes a 400 error to the context and returns
// false so the caller can abort early.
//
// When no filter is configured (the default) the body is returned unchanged and
// the function is effectively free. The protocol argument selects which
// translator family the request belongs to ("openai", "claude", "gemini",
// "codex", "antigravity"); it is matched against the filter's Protocols list.
func ApplyContentFilter(c *gin.Context, rawJSON []byte, cfg *config.SDKConfig, protocol string) ([]byte, bool) {
	if cfg == nil || !cfg.ContentFilter.Enabled {
		return rawJSON, true
	}
	f := contentFilterCache.get(cfg)
	if f == nil {
		return rawJSON, true
	}
	model := gjson.GetBytes(rawJSON, "model").String()
	updated, decision := f.ApplyRequest(rawJSON, model, protocol)
	if decision == nil {
		return updated, true
	}
	if decision.Blocked {
		writeContentFilterBlock(c, decision)
		return updated, false
	}
	return updated, true
}

func writeContentFilterBlock(c *gin.Context, dec *contentfilter.Decision) {
	if c == nil {
		return
	}
	c.AbortWithStatusJSON(http.StatusBadRequest, ErrorResponse{
		Error: ErrorDetail{
			Message: dec.Reason,
			Type:    "invalid_request_error",
			Code:    "content_filter_blocked",
		},
	})
}
