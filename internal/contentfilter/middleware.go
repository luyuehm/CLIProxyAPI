package contentfilter

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// upstreamModelHeader is the header the gateway sets (when known) to carry the
// upstream model name so model-scoped rules can be evaluated.
const upstreamModelHeader = "X-CPA-Upstream-Model"

// Middleware applies content filter rules to every request passing through it.
// It is constructed from a syncer (the live rule source) and an engine. The
// middleware is a pure addition to the gateway: it reads request bodies and
// rewrites responses without modifying upstream handlers.
type Middleware struct {
	syncer *Syncer
	engine *Engine
}

// NewMiddleware creates a content filter middleware backed by the given
// syncer and engine.
func NewMiddleware(syncer *Syncer, engine *Engine) *Middleware {
	return &Middleware{syncer: syncer, engine: engine}
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
		}
		c.Writer = w

		c.Next()

		w.finalize()
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
		logMatch("inbound", res.Matches)
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

func logMatch(direction string, matches []string) {
	if len(matches) == 0 {
		return
	}
	logger.WithField("direction", direction).Infof("content filter matched %d value(s): %v", len(matches), matches)
}