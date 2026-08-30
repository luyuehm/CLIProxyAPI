package contentfilter

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	_ "modernc.org/sqlite"
)

// newTestMiddleware builds a middleware backed by a syncer whose rule set is
// fixed (no KEEPER required). It returns the middleware plus the rules.
func newTestMiddleware(t *testing.T, words []string, pii []PIIType) (*Middleware, []*Rule) {
	t.Helper()
	rules := []*Rule{newTestRule(1, "test-rule", words, pii)}
	engine := NewEngine(true)
	mw := &Middleware{
		syncer: &Syncer{},
		engine: engine,
	}
	// Populate the syncer's active set directly (no Start()).
	mw.syncer.mu.Lock()
	mw.syncer.active = rules
	mw.syncer.mu.Unlock()
	return mw, rules
}

func ginTestEngine(t *testing.T, mw *Middleware) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(mw.Handler())
	return r
}

func TestMiddlewareInboundMasking(t *testing.T) {
	mw, _ := newTestMiddleware(t, []string{"绝密文件"}, []PIIType{PIIPhone})
	r := ginTestEngine(t, mw)

	r.POST("/v1/chat/completions", func(c *gin.Context) {
		// Read the (masked) body the downstream handler would see.
		body, _ := c.GetRawData()
		c.JSON(http.StatusOK, gin.H{"seen": string(body)})
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"gpt-5","messages":[{"content":"绝密文件 手机13800138000"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer x")

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	var resp struct {
		Seen string `json:"seen"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if strings.Contains(resp.Seen, "绝密文件") {
		t.Fatalf("inbound sensitive word leaked through: %q", resp.Seen)
	}
	if strings.Contains(resp.Seen, "13800138000") {
		t.Fatalf("inbound phone leaked through: %q", resp.Seen)
	}
	if !strings.Contains(resp.Seen, "****") {
		t.Fatalf("expected masked body, got %q", resp.Seen)
	}
	t.Logf("downstream handler saw masked body: %q", resp.Seen)
}

func TestMiddlewareOutboundMasking(t *testing.T) {
	mw, _ := newTestMiddleware(t, nil, []PIIType{PIIPhone})
	r := ginTestEngine(t, mw)

	r.POST("/v1/chat/completions", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"content": "您的验证码手机 13800138000 请查收",
		})
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"gpt-5"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer x")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if strings.Contains(rec.Body.String(), "13800138000") {
		t.Fatalf("outbound phone leaked: %q", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "138****8000") {
		t.Fatalf("outbound partial mask missing: %q", rec.Body.String())
	}
	t.Logf("outbound masked body: %s", rec.Body.String())
}

func TestMiddlewareSSEStreaming(t *testing.T) {
	mw, _ := newTestMiddleware(t, nil, []PIIType{PIIPhone})
	r := ginTestEngine(t, mw)

	r.POST("/v1/chat/completions", func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		// Simulate a streaming upstream.
		flusher, _ := c.Writer.(http.Flusher)
		c.Writer.WriteString("data: {\"phone\":\"13800138000\"}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		c.Writer.WriteString("data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"gpt-5","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer x")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	body := rec.Body.String()
	if strings.Contains(body, "13800138000") {
		t.Fatalf("SSE stream phone leaked: %q", body)
	}
	if !strings.Contains(body, "138****8000") {
		t.Fatalf("SSE stream phone not masked: %q", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("SSE framing broken: %q", body)
	}
	t.Logf("SSE masked body: %q", body)
}

// TestMiddlewareAuditEnqueueInbound proves the middleware feeds the audit
// queue with a per-hit row whenever inbound masking fires.
func TestMiddlewareAuditEnqueueInbound(t *testing.T) {
	sidecar := filepath.Join(t.TempDir(), "audit.db")
	audit := NewAudit(AuditEnv{
		HostDBPath:     "/nonexistent/does/not/exist",
		SidecarPath:    sidecar,
		QueueSize:      16,
		WriteTimeout:   2 * time.Second,
	})
	audit.env.ContainerName = ""
	defer audit.Close()

	mw, _ := newTestMiddleware(t, []string{"绝密文件"}, nil)
	mw.audit = audit
	r := ginTestEngine(t, mw)

	r.POST("/v1/chat/completions", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"gpt-5","messages":[{"content":"绝密文件"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// Wait for the audit worker to drain.
	deadline := time.Now().Add(3 * time.Second)
	var wr uint64
	for time.Now().Before(deadline) {
		_, _, w, _ := audit.Stats()
		if w >= 1 {
			wr = w
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if wr < 1 {
		enq, drop, w, fail := audit.Stats()
		t.Fatalf("audit row not written, stats enq=%d drop=%d wr=%d fail=%d", enq, drop, w, fail)
	}

	// Verify the row in the sidecar.
	db, err := sql.Open("sqlite", "file:"+sidecar+"?mode=ro")
	if err != nil {
		t.Fatalf("open sidecar: %v", err)
	}
	defer db.Close()
	row := db.QueryRow(`SELECT rule_id, rule_name, filter_type, raw_preview, client_ip FROM content_filter_logs ORDER BY id DESC LIMIT 1`)
	var (
		rid   int64
		rname string
		ftype string
		raw   string
		ip    string
	)
	if err := row.Scan(&rid, &rname, &ftype, &raw, &ip); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if rname != "test-rule" || ftype != "inbound" || !strings.Contains(raw, "绝密文件") {
		t.Fatalf("unexpected row: rid=%d name=%q ftype=%q raw=%q ip=%q", rid, rname, ftype, raw, ip)
	}
	if ip == "" {
		t.Fatalf("client_ip should be set, got empty")
	}
	t.Logf("audit row: rid=%d name=%q ftype=%q raw=%q ip=%q", rid, rname, ftype, raw, ip)
}
