package policies

import (
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

// logger is the package-level logger.
var logger = logrus.WithField("component", "policies")

// Middleware enforces the delivered per-API-key rate-limit policies on
// data-plane model-serving requests. It is a pure admission gate: it runs after
// authentication (so the API key is already resolved) and before the model
// handler, rejecting over-limit keys with a standard OpenAI-compatible
// 429 + Retry-After. On success it hands through untouched.
type Middleware struct {
	store *Store
	// rpm tracks per-key RPM token buckets.
	rpm sync.Map // apiKey -> *bucket
	// concurrency tracks per-key in-flight requests.
	concurrency sync.Map // apiKey -> *int64
	// pathScopes restricts the middleware to data-plane paths.
	pathScopes []string
}

// MiddlewareOptions configures the policy admission middleware.
type MiddlewareOptions struct {
	// Store supplies the delivered per-key policies. Nil disables enforcement.
	Store *Store
	// PathScopes lists path prefixes the middleware guards. Empty means guard
	// everything (defaults to the data-plane scopes).
	PathScopes []string
}

// defaultDataPlanePathScopes are the model-serving route groups registered in
// server_routes.go. Management and health surfaces are deliberately excluded.
var defaultDataPlanePathScopes = []string{
	"/v1/",
	"/openai/v1/",
	"/v1beta/",
	"/backend-api/codex/",
}

func (o *MiddlewareOptions) withDefaults() {
	if len(o.PathScopes) == 0 {
		o.PathScopes = defaultDataPlanePathScopes
	}
}

// NewMiddleware builds a policy admission gate backed by a Store.
func NewMiddleware(opts MiddlewareOptions) *Middleware {
	opts.withDefaults()
	return &Middleware{
		store:      opts.Store,
		pathScopes: opts.PathScopes,
	}
}

// Handler returns a Gin middleware handler.
func (m *Middleware) Handler() gin.HandlerFunc {
	return func(c *gin.Context) {
		if m == nil || m.store == nil || !m.store.Verified() || !m.guardedPath(c.Request.URL.Path) {
			c.Next()
			return
		}
		key := apiKeyFromContext(c)
		if key == "" {
			c.Next()
			return
		}
		policy, ok := m.store.Get(key)
		if !ok || policy.Revoked {
			c.Next()
			return
		}

		// Concurrency gate first: reject when in-flight requests already meet
		// the delivered concurrency limit.
		if policy.MaxConcurrent > 0 {
			current := m.bumpConcurrency(key, 1)
			if current > policy.MaxConcurrent {
				m.bumpConcurrency(key, -1)
				m.writeTooManyRequests(c, "concurrency limit exceeded")
				return
			}
			defer m.bumpConcurrency(key, -1)
		}

		// RPM token bucket gate.
		if policy.RPMLimit > 0 {
			b := m.bucketFor(key)
			if !b.allow(policy.RPMLimit) {
				m.writeTooManyRequests(c, "rate limit exceeded")
				return
			}
		}

		c.Next()
	}
}

// guardedPath reports whether the request path falls within the guarded scopes.
func (m *Middleware) guardedPath(requestPath string) bool {
	for _, scope := range m.pathScopes {
		if strings.HasPrefix(requestPath, scope) {
			return true
		}
	}
	return false
}

// apiKeyFromContext extracts the resolved API key set by AuthMiddleware.
//
// Because this middleware is mounted globally (before the /v1 route group's
// AuthMiddleware), the "userApiKey" context value is not yet populated on the
// request path. We therefore also resolve the bearer token directly from the
// Authorization header (and its aliases), matching the access-manager's key
// extraction so the delivered KEEPER policy keys line up with what the
// credential provider authenticates.
func apiKeyFromContext(c *gin.Context) string {
	if c == nil {
		return ""
	}
	if value, exists := c.Get("userApiKey"); exists {
		if key, ok := value.(string); ok && key != "" {
			return key
		}
	}
	return bearerFromHeader(c.Request)
}

// bearerFromHeader resolves the API key from the standard auth headers the
// access manager checks: Authorization Bearer, X-Goog-Api-Key, X-Api-Key, and
// the ?key= query parameter.
func bearerFromHeader(r *http.Request) string {
	if r == nil {
		return ""
	}
	for _, candidate := range []string{
		extractBearer(r.Header.Get("Authorization")),
		r.Header.Get("X-Goog-Api-Key"),
		r.Header.Get("X-Api-Key"),
	} {
		if candidate != "" {
			return candidate
		}
	}
	if queryKey := r.URL.Query().Get("key"); queryKey != "" {
		return queryKey
	}
	return ""
}

// extractBearer pulls the token out of an "Authorization: Bearer <token>"
// header, or returns the raw value when there is no Bearer prefix (matching
// the access-manager behaviour for non-standard auth material).
func extractBearer(header string) string {
	header = strings.TrimSpace(header)
	if header == "" {
		return ""
	}
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 {
		return header
	}
	if !strings.EqualFold(parts[0], "bearer") {
		return header
	}
	return strings.TrimSpace(parts[1])
}

// bucket is a fixed-window per-second token bucket. The window resets on the
// second boundary, so it is deterministic and cheap.
type bucket struct {
	mu       sync.Mutex
	window   int64
	lastFill time.Time
	tokens   float64
}

// allow consumes one token from the bucket, refilling at the configured rate
// over a 1-second window. It is safe for concurrent use.
func (b *bucket) allow(rate int64) bool {
	if rate <= 0 {
		return true
	}
	now := time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	window := now.Unix()
	if window != b.window {
		b.window = window
		b.lastFill = now
		b.tokens = float64(rate)
	}
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

func (m *Middleware) bucketFor(key string) *bucket {
	value, _ := m.rpm.LoadOrStore(key, &bucket{})
	return value.(*bucket)
}

func (m *Middleware) bumpConcurrency(key string, delta int64) int64 {
	value, _ := m.concurrency.LoadOrStore(key, new(int64))
	return atomic.AddInt64(value.(*int64), delta)
}

// writeTooManyRequests writes a standard 429 with an OpenAI-compatible error
// body and a Retry-After of 1 second.
func (m *Middleware) writeTooManyRequests(c *gin.Context, message string) {
	c.Header("Retry-After", "1")
	c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
		"error": gin.H{
			"message": message,
			"type":    "rate_limit_error",
			"code":    "rate_limit_exceeded",
		},
	})
}