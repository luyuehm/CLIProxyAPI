package contentfilter

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// upstreamModelHeader is the header the gateway sets (when known) to carry the
// upstream model name so model-scoped rules can be evaluated.
const upstreamModelHeader = "X-CPA-Upstream-Model"

// auditPreviewMax caps the size of raw / filtered previews stored per audit
// row so a single huge request does not balloon the audit table.
const auditPreviewMax = 4 << 10 // 4 KiB

// Middleware applies content filter rules to every request passing through it.
// It is constructed from a syncer (the live rule source) and an engine. The
// middleware is a pure addition to the gateway: it reads request bodies and
// rewrites responses without modifying upstream handlers.
type Middleware struct {
	syncer *Syncer
	engine *Engine
	audit  *Audit

	// currentCtx is set per request by Handler() and read by enqueueOutboundAudit
	// when the response writer finalizes the outbound body. Using a per-mw
	// pointer keeps Handler()'s closure simple and the audit path branchless.
	currentCtx *gin.Context
}

// NewMiddleware creates a content filter middleware backed by the given
// syncer and engine. Pass nil for audit to disable audit writes.
func NewMiddleware(syncer *Syncer, engine *Engine, audit *Audit) *Middleware {
	return &Middleware{syncer: syncer, engine: engine, audit: audit}
}

// Handler returns a Gin middleware handler.
func (m *Middleware) Handler() gin.HandlerFunc {
	return func(c *gin.Context) {
		if m.syncer == nil || m.syncer.Stale() {
			c.Next()
			return
		}
		// The syncer's rule set is immutable once returned.
		rules := m.syncer.Rules()
		if len(rules) == 0 {
			c.Next()
			return
		}

		m.currentCtx = c

		model, stream := m.filterRequest(c, rules)
		if model == "" {
			model = strings.TrimSpace(c.Request.Header.Get(upstreamModelHeader))
		}

		w := &filterResponseWriter{
			ResponseWriter: c.Writer,
			engine:         m.engine,
			rules:          rules,
			model:          model,
			status:         http.StatusOK,
			wantsStream:    stream,
			mw:             m,
		}
		c.Writer = w

		c.Next()

		w.finalize()
		m.currentCtx = nil
	}
}

// filterRequest intercepts the inbound request body, applies inbound masking,
// and restores the masked body so downstream handlers read it. It returns the
// upstream model parsed from the body (if present) and whether the request
// asked for a streaming response. Multipart uploads and compressed bodies are
// left untouched.
func (m *Middleware) filterRequest(c *gin.Context, rules []*Rule) (string, bool) {
	if c.Request == nil || c.Request.Body == nil || c.Request.Body == http.NoBody {
		return "", false
	}
	ct := strings.ToLower(strings.TrimSpace(c.Request.Header.Get("Content-Type")))
	if strings.HasPrefix(ct, "multipart/") {
		return "", false
	}
	// Compressed request bodies cannot be masked without decompressing them;
	// leave them to pass through untouched rather than corrupting the payload.
	if enc := strings.ToLower(strings.TrimSpace(c.Request.Header.Get("Content-Encoding"))); enc != "" && enc != "identity" {
		return "", false
	}

	body, err := io.ReadAll(c.Request.Body)
	if err != nil || len(body) == 0 {
		return "", false
	}
	_ = c.Request.Body.Close()

	stream := bytes.Contains(body, []byte(`"stream": true`)) || bytes.Contains(body, []byte(`"stream":true`))

	model := modelFromJSON(body)
	res := m.engine.Apply(rules, string(body), true, model)
	masked := []byte(res.Text)
	if res.Changed {
		m.enqueueAudit(c, res, "inbound", model, string(body), res.Text)
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(masked))
	c.Request.ContentLength = int64(len(masked))
	c.Set(gin.BodyBytesKey, masked)
	return model, stream
}

// modelFromJSON reads the "model" field from a JSON payload. It is tolerant of
// absent or invalid bodies so filtering never blocks a request.
func modelFromJSON(b []byte) string {
	if len(b) == 0 || !json.Valid(b) {
		return ""
	}
	var payload struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(b, &payload); err != nil {
		return ""
	}
	return strings.TrimSpace(payload.Model)
}

// enqueueAudit submits one audit row per matched rule. It is non-blocking:
// the audit queue is bounded and overflow is dropped with a warning. The
// caller must always pass rich context (direction/model/raw/filtered) so the
// KEEPER audit table is useful to operators.
func (m *Middleware) enqueueAudit(c *gin.Context, res Result, direction, model, raw, filtered string) {
	if m.audit == nil {
		logMatch(direction, res.Matches)
		return
	}
	logger.WithField("direction", direction).Infof("content filter matched %d value(s) across %d rule(s)", len(res.Matches), len(res.RuleHits))

	clientIP, userID := clientAndUser(c)
	for _, hit := range res.RuleHits {
		m.audit.Enqueue(AuditRow{
			RuleID:          hit.RuleID,
			RuleName:        hit.RuleName,
			FilterType:      direction,
			MatchCount:      len(hit.Matches),
			Matches:         strings.Join(hit.Matches, ","),
			Action:          "mask",
			Model:           model,
			ClientIP:        clientIP,
			UserID:          userID,
			RawPreview:      truncatePreview(raw, auditPreviewMax),
			FilteredPreview: truncatePreview(filtered, auditPreviewMax),
			CreatedAt:       time.Now().UTC(),
		})
	}
}

// enqueueOutboundAudit is the response-side counterpart of enqueueAudit. It
// uses the middleware's currentCtx (set by Handler) to read the request's
// client IP and user id. Streaming responses are aggregated by the response
// writer; the streamed text is the raw preview and the masked text is the
// filtered preview.
func (m *Middleware) enqueueOutboundAudit(res *Result, model, raw string) {
	if m.audit == nil || res == nil || !res.Changed {
		return
	}
	c := m.currentCtx
	if c == nil {
		return
	}
	clientIP, userID := clientAndUser(c)
	for _, hit := range res.RuleHits {
		m.audit.Enqueue(AuditRow{
			RuleID:          hit.RuleID,
			RuleName:        hit.RuleName,
			FilterType:      "outbound",
			MatchCount:      len(hit.Matches),
			Matches:         strings.Join(hit.Matches, ","),
			Action:          "mask",
			Model:           model,
			ClientIP:        clientIP,
			UserID:          userID,
			RawPreview:      truncatePreview(raw, auditPreviewMax),
			FilteredPreview: truncatePreview(res.Text, auditPreviewMax),
			CreatedAt:       time.Now().UTC(),
		})
	}
}

// clientAndUser returns the request's client IP and a pseudonymised
// per-caller identifier derived from the Authorization header. The raw
// token is never persisted to the audit log.
func clientAndUser(c *gin.Context) (string, string) {
	if c == nil {
		return "", ""
	}
	ip := c.ClientIP()
	auth := c.GetHeader("Authorization")
	var user string
	if auth != "" {
		user = "auth:" + shortHash(auth)
	}
	return ip, user
}

// logMatch is the audit-disabled fallback: a single info-level line per
// direction. It is called only when no audit writer is configured.
func logMatch(direction string, matches []string) {
	if len(matches) == 0 {
		return
	}
	logger.WithField("direction", direction).Infof("content filter matched %d value(s): %v", len(matches), matches)
}