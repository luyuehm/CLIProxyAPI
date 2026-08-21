package handlers

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cache"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/tidwall/gjson"
)

const (
	// CacheHitHeader is set on the response when a cached result is returned.
	CacheHitHeader = "X-Cache"
	// CacheHitValue indicates a cache hit.
	CacheHitValue = "HIT"
	// CacheMissValue indicates a cache miss.
	CacheMissValue = "MISS"
	// CacheBypassHeader is the request header that forces a bypass.
	CacheBypassHeader = "Cache-Control"
	// CacheBypassValue is the value that forces a bypass.
	CacheBypassValue = "no-cache"
	// CacheNoStoreValue is the value that prevents caching.
	CacheNoStoreValue = "no-store"
)

// PromptCacheRequest holds the cacheable request context.
type PromptCacheRequest struct {
	Model       string
	HandlerType string
	RawJSON     []byte
}

// BuildPromptCacheKey constructs a cache key from the request body.
// Returns nil key if the request is not cacheable (streaming, etc.).
func BuildPromptCacheKey(rawJSON []byte, model string) *cache.PromptCacheKey {
	if len(rawJSON) == 0 || model == "" {
		return nil
	}

	// Only cache chat completions/messages-like requests with a messages array
	messages := gjson.GetBytes(rawJSON, "messages")
	if !messages.Exists() || !messages.IsArray() || len(messages.Array()) == 0 {
		return nil
	}

	// Do not cache streaming requests
	stream := gjson.GetBytes(rawJSON, "stream")
	if stream.Exists() && stream.Bool() {
		return nil
	}

	temperature := gjson.GetBytes(rawJSON, "temperature").Float()
	if temperature < 0 {
		temperature = 0
	}
	if temperature > 0 {
		// Only cache temperature=0 (deterministic) requests
		// Allow a small epsilon for floating point
		if temperature > 0.01 {
			return nil
		}
		temperature = 0
	}

	return &cache.PromptCacheKey{
		Model:       model,
		Temperature: temperature,
		Messages:    messages.Raw,
	}
}

// ShouldBypassCache checks whether the request headers indicate a cache bypass.
func ShouldBypassCache(c *gin.Context) bool {
	if c == nil || c.Request == nil {
		return false
	}
	cc := strings.TrimSpace(c.Request.Header.Get(CacheBypassHeader))
	if strings.EqualFold(cc, CacheBypassValue) || strings.EqualFold(cc, CacheNoStoreValue) {
		return true
	}
	return false
}

// ShouldSkipCacheStore checks whether the response should not be stored.
func ShouldSkipCacheStore(c *gin.Context) bool {
	if c == nil || c.Request == nil {
		return false
	}
	cc := strings.TrimSpace(c.Request.Header.Get(CacheBypassHeader))
	return strings.EqualFold(cc, CacheNoStoreValue)
}

// SetCacheHitHeader sets the X-Cache header on the response.
func SetCacheHitHeader(c *gin.Context, hit bool) {
	if c == nil {
		return
	}
	if hit {
		c.Header(CacheHitHeader, CacheHitValue)
	} else {
		c.Header(CacheHitHeader, CacheMissValue)
	}
}

// TryPromptCache checks the cache for a matching request. If found, it writes the
// cached response and returns true. Otherwise, it returns false.
func TryPromptCache(c *gin.Context, key *cache.PromptCacheKey) bool {
	if key == nil || !cache.HasPromptCache() {
		return false
	}
	if ShouldBypassCache(c) {
		return false
	}

	body := cache.GetPromptCache(c.Request.Context(), *key)
	if body == nil {
		return false
	}

	SetCacheHitHeader(c, true)
	_, _ = c.Writer.Write(body)
	return true
}

// StorePromptCache stores a response body in the cache after a successful request.
func StorePromptCache(ctx interface{ Gin() *gin.Context }, c *gin.Context, key *cache.PromptCacheKey, body []byte) {
	if key == nil || !cache.HasPromptCache() || len(body) == 0 {
		return
	}
	if c != nil && ShouldSkipCacheStore(c) {
		return
	}

	ginCtx := c
	if ginCtx == nil {
		if requestCtx, ok := ctx.(interface{ Gin() *gin.Context }); ok {
			ginCtx = requestCtx.Gin()
		}
	}
	if ginCtx != nil && ShouldSkipCacheStore(ginCtx) {
		return
	}

	cache.SetPromptCache(c.Request.Context(), *key, body)
}

// CheckCacheBeforeExecute checks the prompt cache before executing a request.
// Returns the cached body and true if found; otherwise nil and false.
// The caller should write the body and return when true.
func CheckCacheBeforeExecute(c *gin.Context, key *cache.PromptCacheKey) ([]byte, bool) {
	if key == nil || !cache.HasPromptCache() {
		return nil, false
	}
	if ShouldBypassCache(c) {
		return nil, false
	}

	body := cache.GetPromptCache(c.Request.Context(), *key)
	if body == nil {
		return nil, false
	}
	return body, true
}

// ExecuteWithPromptCache wraps a non-streaming execution call with prompt cache.
// It checks the cache first, and on cache miss it executes, caches, and returns.
// The execFn is the actual execution function.
func (h *BaseAPIHandler) ExecuteWithPromptCache(
	c *gin.Context,
	key *cache.PromptCacheKey,
	execFn func() ([]byte, http.Header, *interfaces.ErrorMessage),
) ([]byte, http.Header, *interfaces.ErrorMessage) {
	// Check cache first
	if body, hit := CheckCacheBeforeExecute(c, key); hit {
		SetCacheHitHeader(c, true)
		return body, nil, nil
	}

	SetCacheHitHeader(c, false)
	body, upstreamHeaders, errMsg := execFn()
	if errMsg != nil {
		return body, upstreamHeaders, errMsg
	}

	// Store in cache on success
	StorePromptCache(nil, c, key, body)
	return body, upstreamHeaders, nil
}

// ExtractMinimalCacheHeaders returns a minimal set of upstream headers
// that should be preserved for a cache hit response.
func ExtractMinimalCacheHeaders(headers http.Header) http.Header {
	if headers == nil {
		return nil
	}
	out := make(http.Header)
	for _, key := range []string{
		"Content-Type",
		"X-Request-Id",
		"OpenAI-Request-Id",
	} {
		if v := headers.Get(key); v != "" {
			out.Set(key, v)
		}
	}
	return out
}

// BuildCacheKeyFromBody is a convenience function that extracts model and builds
// a cache key from the raw JSON body.
func BuildCacheKeyFromBody(rawJSON []byte) *cache.PromptCacheKey {
	model := gjson.GetBytes(rawJSON, "model").String()
	return BuildPromptCacheKey(rawJSON, model)
}

// parseCacheableJSONBody checks if the JSON body is a non-streaming
// chat completions-style request that can be cached.
func parseCacheableJSONBody(rawJSON []byte) (string, bool) {
	if len(rawJSON) == 0 || !json.Valid(rawJSON) {
		return "", false
	}
	model := strings.TrimSpace(gjson.GetBytes(rawJSON, "model").String())
	if model == "" {
		return "", false
	}
	stream := gjson.GetBytes(rawJSON, "stream")
	if stream.Exists() && stream.Bool() {
		return "", false
	}
	messages := gjson.GetBytes(rawJSON, "messages")
	if !messages.Exists() || !messages.IsArray() || len(messages.Array()) == 0 {
		return "", false
	}
	return model, true
}
