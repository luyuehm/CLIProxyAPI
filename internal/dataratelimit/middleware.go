package dataratelimit

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// Middleware enforces per-API-key data-plane rate limits and the KEEPER
// quota-exhausted circuit breaker on model-serving requests.
//
// It is a pure admission gate: it runs after authentication (so the API key is
// already resolved) and before the model handler, rejecting over-limit and
// quota-exhausted keys with a standard OpenAI-compatible 429 + Retry-After.
// On success it hands through untouched, so it never modifies the request body
// or the upstream response.
type Middleware struct {
	limiter   *Limiter
	blacklist *Blacklist

	// pathScopes restricts the middleware to data-plane model-serving paths.
	// Requests outside these prefixes pass through unadmitted (no rate
	// applied), matching upstream behaviour for management/health surfaces.
	pathScopes []string
}

// MiddlewareOptions configures the data-plane rate-limit middleware.
type MiddlewareOptions struct {
	// Limiter applies per-key RPM and concurrency limits. Nil disables the
	// local limiter (only the blacklist gates).
	Limiter *Limiter
	// Blacklist breaks the circuit for quota-exhausted keys and supplies
	// KEEPER-published per-key limits. Nil disables both.
	Blacklist *Blacklist
	// PathScopes lists path prefixes the middleware guards. Empty means
	// "guard everything".
	PathScopes []string
}

func (o *MiddlewareOptions) withDefaults() {
	if len(o.PathScopes) == 0 {
		o.PathScopes = defaultDataPlanePathScopes
	}
}

// defaultDataPlanePathScopes are the model-serving route groups registered in
// server_routes.go. Management and health surfaces are deliberately excluded.
var defaultDataPlanePathScopes = []string{
	"/v1/",
	"/openai/v1/",
	"/v1beta/",
	"/backend-api/codex/",
}

// NewMiddleware builds a data-plane rate-limit admission gate.
func NewMiddleware(opts MiddlewareOptions) *Middleware {
	opts.withDefaults()
	return &Middleware{
		limiter:    opts.Limiter,
		blacklist:  opts.Blacklist,
		pathScopes: opts.PathScopes,
	}
}

// Handler returns a Gin middleware handler.
func (m *Middleware) Handler() gin.HandlerFunc {
	return func(c *gin.Context) {
		if m == nil || !m.guardedPath(c.Request.URL.Path) {
			c.Next()
			return
		}
		key := apiKeyFromContext(c)
		if key == "" {
			// No authenticated key (or auth disabled) — nothing to rate-limit
			// by key; pass through untouched to preserve upstream behaviour.
			c.Next()
			return
		}

		// Circuit breaker first: a key KEEPER reports as quota-exhausted is
		// rejected before it can consume RPM/concurrency tokens.
		if m.blacklist != nil && m.blacklist.IsBlocked(key) {
			m.writeTooManyRequests(c, "quota exhausted")
			return
		}

		// Apply KEEPER-published per-key limits when available, else defaults.
		var limits KeyLimits
		if m.blacklist != nil {
			if published, ok := m.blacklist.Limits(key); ok {
				limits = published
			}
		}

		if m.limiter == nil {
			c.Next()
			return
		}
		if allowed, retryAfter := m.limiter.AllowWithLimits(key, limits); !allowed {
			m.writeTooManyRequestsRetryAfter(c, "rate limit exceeded", retryAfter)
			return
		}
		// Release the concurrency slot when the request finishes. Because the
		// deferred Release runs after Next(), it always balances the Allow —
		// on both the normal and the Abort paths.
		defer m.limiter.Release(key)

		c.Next()
	}
}

// guardedPath reports whether the request path falls within the guarded
// data-plane scopes.
func (m *Middleware) guardedPath(requestPath string) bool {
	for _, scope := range m.pathScopes {
		if strings.HasPrefix(requestPath, scope) {
			return true
		}
	}
	return false
}

// apiKeyFromContext extracts the resolved API key from the access-manager
// context value set by AuthMiddleware. When authentication is disabled or the
// provider did not resolve a key, it returns "".
func apiKeyFromContext(c *gin.Context) string {
	if c == nil {
		return ""
	}
	value, exists := c.Get("userApiKey")
	if !exists {
		return ""
	}
	key, ok := value.(string)
	if !ok {
		return ""
	}
	return key
}

// writeTooManyRequests writes a standard 429 with an OpenAI-compatible error
// body and a Retry-After of 1 second.
func (m *Middleware) writeTooManyRequests(c *gin.Context, message string) {
	m.writeTooManyRequestsRetryAfter(c, message, time.Second)
}

// writeTooManyRequestsRetryAfter writes a 429, an OpenAI-compatible error body,
// and a Retry-After header (in whole seconds, per RFC 7231).
func (m *Middleware) writeTooManyRequestsRetryAfter(c *gin.Context, message string, retryAfter time.Duration) {
	if c == nil {
		return
	}
	if retryAfter < time.Second {
		retryAfter = time.Second
	}
	seconds := int64(retryAfter.Round(time.Second) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	c.Header("Retry-After", strconv.FormatInt(seconds, 10))
	c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
		"error": gin.H{
			"message": message,
			"type":    "rate_limit_error",
			"code":    "rate_limit_exceeded",
		},
	})
}
