package routeboundary

import (
	"bytes"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// logger is the package-level logger.
var logger = logrus.WithField("component", "routeboundary")

// transportAltValues are the ?alt= values the gateway itself passes to
// executors as transport/format markers (streaming, responses compaction,
// Gemini $alt). They are never provider selections, so an admission gate
// must not 403 a key for "json" or "sse" when its boundary excludes them.
// Any other alt value is OpenRouter-style provider selection (?alt=<provider>)
// and is checked against the boundary.
var transportAltValues = map[string]struct{}{
	"sse":               {},
	"json":              {},
	"streamed-json":     {},
	"b64":               {},
	"responses/compact": {},
}

// Middleware enforces the delivered route boundaries at admission, on the
// request surface the gateway can see before the model handler runs: an
// explicit out-of-boundary provider selection (?alt=<p> naming a provider,
// or the X-OpenRouter-Provider header) is rejected with 403
// route_boundary_violation, and an override header on a key without the
// permission is rejected the same way. The provider pool narrowing itself
// happens in the execution gate (Enforcer.ApplyBoundary), because the pool
// only exists after model resolution.
//
// The middleware is a pure admission gate mounted through the pre-reserved
// api.WithMiddleware() extension point (the policies precedent): it reads a
// snapshot under a read lock, never does network I/O on the request path,
// and passes everything through when the channel is unconfigured or the
// store is unverified.
type Middleware struct {
	store *Store
	audit *Audit
	// pathScopes restricts the middleware to data-plane paths.
	pathScopes []string
}

// MiddlewareOptions configures the boundary admission middleware.
type MiddlewareOptions struct {
	// Store supplies the delivered per-key boundaries. Nil disables
	// enforcement.
	Store *Store
	// Audit receives out-of-boundary override events; nil disables audit
	// posts (overrides are still granted and stamped, KEEPER just sees no
	// events).
	Audit *Audit
	// PathScopes lists path prefixes the middleware guards. Empty means
	// guard the data-plane scopes.
	PathScopes []string
}

// defaultDataPlanePathScopes are the model-serving route groups registered
// in server_routes.go. Management and health surfaces are deliberately
// excluded, mirroring the policies channel.
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

// NewMiddleware builds a boundary admission gate backed by a Store.
func NewMiddleware(opts MiddlewareOptions) *Middleware {
	opts.withDefaults()
	return &Middleware{
		store:      opts.Store,
		audit:      opts.Audit,
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
		key := apiKeyFromGin(c)
		if key == "" {
			// No resolvable caller key: nothing to look up, pass through.
			c.Next()
			return
		}
		boundary, hasBoundary := m.store.Get(key)
		if !hasBoundary || !boundary.Constrains() {
			// The key carries no hard boundary: routing is untouched, but
			// the audit header still marks that the gate ran for this key
			// (candidate-3 observability of the shared contract).
			c.Header(HeaderPolicyApplied, "false")
			c.Next()
			return
		}

		// Constrained request: the policy applied, whatever the outcome.
		c.Header(HeaderPolicyApplied, "true")

		// Override header: grant the permission here (the pool pinning and
		// the response header happen at execution time in the enforcer),
		// or 403 when the key lacks the permission.
		if override := strings.TrimSpace(c.GetHeader(HeaderBoundary)); override != "" {
			if !boundary.OverrideAllowed {
				logger.WithField("provider", override).Debug("routeboundary: override header rejected (key lacks override permission)")
				m.writeViolation(c, http.StatusForbidden,
					override+" is not within the configured route boundary")
				return
			}
			c.Set(overrideCtxKey, override)
			// The audit event names the request's model; the body is read
			// (and restored) here at admission, before the handler
			// consumes it.
			model := modelFromContext(c)
			c.Next()
			// Best-effort audit of the granted out-of-boundary pass,
			// enqueued after the response so it carries the final status.
			// A missed audit row must never affect the response (the
			// writer is fire-and-forget).
			m.recordOverrideAudit(c, key, boundary, override, model)
			return
		}
		// Explicit out-of-boundary provider selection: 403, naming the
		// provider (scenario 3 of the shared contract).
		if provider := m.explicitSelection(c); provider != "" && !boundary.Allows(provider) {
			logger.WithField("provider", provider).Debug("routeboundary: explicit provider outside boundary rejected at admission")
			m.writeViolation(c, http.StatusForbidden,
				provider+" is not within the configured route boundary")
			return
		}

		c.Next()
	}
}

// guardedPath reports whether the request path falls within the guarded
// scopes.
func (m *Middleware) guardedPath(requestPath string) bool {
	for _, scope := range m.pathScopes {
		if strings.HasPrefix(requestPath, scope) {
			return true
		}
	}
	return false
}

// explicitSelection returns the client's explicit provider choice visible at
// admission: the ?alt= query value when it names a provider (anything that
// is not a known transport marker), or the X-OpenRouter-Provider header.
func (m *Middleware) explicitSelection(c *gin.Context) string {
	if provider := strings.TrimSpace(c.GetHeader(HeaderExplicitProvider)); provider != "" {
		return provider
	}
	alt := strings.TrimSpace(c.Query("alt"))
	if alt == "" {
		alt = strings.TrimSpace(c.Query("$alt"))
	}
	if alt == "" || isTransportAlt(alt) {
		return ""
	}
	return alt
}

// recordOverrideAudit enqueues the out-of-boundary override event after the
// request has completed, so the event carries the final response status.
// The model is captured at admission (before the handler consumes the
// body). Best-effort: a missed audit row must never affect the response
// (the writer is fire-and-forget and drops on a full queue).
func (m *Middleware) recordOverrideAudit(c *gin.Context, key string, boundary Boundary, provider, model string) {
	if m.audit == nil {
		return
	}
	status := http.StatusOK
	if c != nil && c.Writer != nil {
		status = c.Writer.Status()
	}
	boundaryList := boundary.AllowedProviders
	if boundaryList == nil {
		boundaryList = []string{}
	}
	m.audit.Record(AuditEvent{
		APIKey:           key,
		KeyAlias:         boundary.KeyAlias,
		Model:            model,
		Boundary:         boundaryList,
		OverrideProvider: provider,
		Action:           "override",
		StatusCode:       status,
	})
}

// modelFromContext extracts the "model" field from the request body so the
// audit event names what the override request targeted. The body is read
// and restored (the contentfilter precedent) so the model handler reads it
// unchanged; multipart bodies and read errors leave it as "".
func modelFromContext(c *gin.Context) string {
	if c == nil || c.Request == nil || c.Request.Body == nil || c.Request.Body == http.NoBody {
		return ""
	}
	contentType := strings.ToLower(c.Request.Header.Get("Content-Type"))
	if strings.HasPrefix(contentType, "multipart/") || strings.HasPrefix(contentType, "application/x-www-form-urlencoded") {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 4<<20))
	if err != nil {
		c.Request.Body = http.NoBody
		return ""
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	return gjson.GetBytes(body, "model").String()
}

// writeViolation writes the contract-mandated 403 body.
func (m *Middleware) writeViolation(c *gin.Context, status int, message string) {
	c.Header("Content-Type", "application/json")
	c.AbortWithStatusJSON(status, violationErrorMap(message))
}
