package contentfilter

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// TestExportHTTPHandlerEndToEnd drives ExportHTTPHandler through Gin with a
// real sidecar db, asserting the HTTP download contract: masked output,
// filters honoured, Content-Disposition present, and unsupported format 400.
func TestExportHTTPHandlerEndToEnd(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dir := t.TempDir()
	sidecar := filepath.Join(dir, "audit.db")
	db, err := sql.Open("sqlite", sidecar)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := ensureSidecarSchema(db); err != nil {
		t.Fatalf("schema: %v", err)
	}
	now := time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC)
	rows := []AuditRow{
		{RuleID: 1, RuleName: "phone", FilterType: "inbound", MatchCount: 1,
			Matches: `["13812345678"]`, Action: "mask", Model: "gpt-5",
			ClientIP: "10.0.0.1", UserID: "auth:abc",
			RawPreview: "联系电话 13812345678 请查收", FilteredPreview: "联系电话 138****5678 请查收",
			CreatedAt: now},
		{RuleID: 2, RuleName: "email", FilterType: "outbound", MatchCount: 1,
			Matches: `["alice@example.com"]`, Action: "mask", Model: "gpt-5",
			ClientIP: "10.0.0.2", UserID: "auth:def",
			RawPreview: "联系 alice@example.com", FilteredPreview: "联系 a***@*******.com",
			CreatedAt: now.Add(time.Minute)},
	}
	// The sidecar must also hold the rules table (production KEEPER app.db
	// always has it) so the handler's re-masking engine can load rules.
	if _, err := db.Exec(`CREATE TABLE content_filter_rules (
		id integer PRIMARY KEY AUTOINCREMENT,
		name text NOT NULL,
		description text,
		enabled integer NOT NULL DEFAULT 1,
		scenario text,
		action text,
		sensitive_words text,
		pii_types text,
		white_list text,
		models text,
		priority integer NOT NULL DEFAULT 0
	)`); err != nil {
		t.Fatalf("create rules table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO content_filter_rules
		(name, enabled, action, pii_types, priority) VALUES
		('phone', 1, 'mask', 'phone', 10),
		('email', 1, 'mask', 'email', 5)`); err != nil {
		t.Fatalf("insert rules: %v", err)
	}
	if err := insertLogBatch(db, rows); err != nil {
		t.Fatalf("insert: %v", err)
	}
	db.Close()

	src := ExportSource{SidecarPath: sidecar}
	handler := ExportHTTPHandler(src)
	router := gin.New()
	router.GET("/export", handler)

	t.Run("csv_masks_sensitive_columns", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/export?format=csv", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
			t.Fatalf("content-type = %q, want text/csv", ct)
		}
		if !strings.Contains(rec.Header().Get("Content-Disposition"), "attachment") {
			t.Fatalf("missing attachment disposition: %q", rec.Header().Get("Content-Disposition"))
		}
		body := rec.Body.String()
		// The raw phone/email must NOT appear unmasked in the export.
		if strings.Contains(body, "13812345678") {
			t.Fatalf("CSV leaked the raw phone: %q", body)
		}
		if strings.Contains(body, "alice@example.com") {
			t.Fatalf("CSV leaked the raw email: %q", body)
		}
	})

	t.Run("json_format", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/export?format=json", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
		}
		if !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
			t.Fatalf("content-type = %q, want application/json", rec.Header().Get("Content-Type"))
		}
		if strings.Contains(rec.Body.String(), "13812345678") || strings.Contains(rec.Body.String(), "alice@example.com") {
			t.Fatalf("JSON leaked raw sensitive values: %q", rec.Body.String())
		}
	})

	t.Run("unsupported_format_400", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/export?format=xlsx", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("filter_model", func(t *testing.T) {
		// A model filter for a model with no rows still succeeds with a body.
		req := httptest.NewRequest(http.MethodGet, "/export?format=csv&model=no-such-model&limit=1000", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
		}
	})
}