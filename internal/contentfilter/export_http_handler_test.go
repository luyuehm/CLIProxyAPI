package contentfilter

import (
	"database/sql"
	"encoding/json"
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

// TestExportNULLColumnsSuccess is the RIC-457 regression test. A content
// filter log row recorded before a rule existed (or later soft-deleted) can
// carry rule_id IS NULL / action IS NULL / match_count IS NULL. Before the
// IFNULL guards, rows.Scan aborted on the first such row and the CSV came out
// 0 bytes. The export must instead return every row with sane defaults.
func TestExportNULLColumnsSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dir := t.TempDir()
	sidecar := filepath.Join(dir, "audit.db")
	db, err := sql.Open("sqlite", sidecar)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Deliberately create the log table with NULL-able action / match_count:
	// insertLogBatch would normalise empty fields away, but the corruption the
	// bug hit is historical rows where the KEEPER side wrote literal NULLs.
	// The schema mirrors production (rule_id nullable) minus the NOT NULL
	// constraints so the fixture can insert genuine NULLs in all three guards.
	if _, err := db.Exec(`CREATE TABLE content_filter_logs (
		id integer PRIMARY KEY AUTOINCREMENT,
		rule_id integer,
		rule_name text,
		filter_type text,
		match_count integer,
		matches text,
		action text,
		model text,
		client_ip text,
		user_id text,
		raw_preview text,
		filtered_preview text,
		created_at datetime
	)`); err != nil {
		t.Fatalf("create nullable log table: %v", err)
	}
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
	// Normal masked row.
	if _, err := db.Exec(`INSERT INTO content_filter_logs
		(rule_id, rule_name, filter_type, match_count, matches, action, model, client_ip, user_id, raw_preview, filtered_preview, created_at)
		VALUES (1, 'phone', 'inbound', 1, '["13812345678"]', 'mask', 'gpt-5', '10.0.0.1', 'auth:abc',
			'联系电话 13812345678', '联系电话 138****5678', '2026-08-20 09:30:00')`); err != nil {
		t.Fatalf("insert normal row: %v", err)
	}
	// Historical row where rule_id, action and match_count are all NULL.
	if _, err := db.Exec(`INSERT INTO content_filter_logs
		(rule_name, filter_type, model, raw_preview, filtered_preview, created_at)
		VALUES ('', 'outbound', 'gpt-5', '未命中任何规则', '', '2026-08-20 09:31:00')`); err != nil {
		t.Fatalf("insert NULL row: %v", err)
	}
	// The KEEPER app.db always has the rules table so the handler's
	// re-masking engine can load rules.
	if _, err := db.Exec(`INSERT INTO content_filter_rules
		(name, enabled, action, pii_types, priority) VALUES
		('phone', 1, 'mask', 'phone', 10)`); err != nil {
		t.Fatalf("insert rules: %v", err)
	}
	// Confirm the fixture really holds NULLs in the guarded columns — the shape
	// the bug aborted on.
	var cnt int
	if err := db.QueryRow(`SELECT COUNT(*) FROM content_filter_logs
		WHERE rule_id IS NULL OR action IS NULL OR match_count IS NULL`).Scan(&cnt); err != nil {
		t.Fatalf("verify NULL row: %v", err)
	}
	if cnt < 1 {
		t.Fatalf("fixture did not produce a NULL column row")
	}

	src := ExportSource{SidecarPath: sidecar}
	handler := ExportHTTPHandler(src)
	router := gin.New()
	router.GET("/export", handler)

	t.Run("csv_null_columns_success", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/export?format=csv", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		// Header + one data row for each of the 2 log rows, including the
		// NULL-column row.
		wantLines := 3
		if got := strings.Count(body, "\n"); got != wantLines {
			t.Fatalf("CSV line count = %d, want %d; body=%q", got, wantLines, body)
		}
		if !strings.Contains(body, "rule_id") || !strings.Contains(body, "action") {
			t.Fatalf("CSV missing header: %q", body)
		}
		// The NULL action row must appear with an empty action column, proving
		// the row was scanned instead of aborting the stream.
		if !strings.Contains(body, "未命中任何规则") {
			t.Fatalf("CSV missing NULL-column data row: %q", body)
		}
	})

	t.Run("json_null_columns_success", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/export?format=json", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
		}
		var records []ExportRecord
		if err := json.Unmarshal(rec.Body.Bytes(), &records); err != nil {
			t.Fatalf("invalid JSON export: %v; body=%q", err, rec.Body.String())
		}
		if len(records) != 2 {
			t.Fatalf("JSON row count = %d, want 2; body=%q", len(records), rec.Body.String())
		}
		var nullRow *ExportRecord
		for i := range records {
			if records[i].RuleID == 0 && records[i].Action == "" {
				nullRow = &records[i]
			}
		}
		if nullRow == nil {
			t.Fatalf("JSON missing NULL-column row: %+v", records)
		}
		if nullRow.MatchCount != 0 {
			t.Fatalf("NULL match_count = %d, want 0", nullRow.MatchCount)
		}
	})

	t.Run("jsonl_null_columns_success", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/export?format=jsonl", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
		}
		lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
		if len(lines) != 2 {
			t.Fatalf("JSONL line count = %d, want 2; body=%q", len(lines), rec.Body.String())
		}
	})
}
// TestExportHTTPHandlerBlockedWhenLicenseInvalid asserts RIC-476 degradation
// of the audit-export enterprise surface: with an invalid KEEPER license the
// handler refuses with 403 and X-License-Status: blocked instead of silently
// serving the download.
func TestExportHTTPHandlerBlockedWhenLicenseInvalid(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Hard-fail the shared probe singleton for this test so ExportHTTPHandler's
	// RIC-476 gate observes an invalid license. Restore afterwards so sibling
	// tests (which rely on an open shared probe) are unaffected.
	sharedProbe := NewLicenseProbe("http://127.0.0.1:1", "k")
	sharedProbe.updateState(false, licenseStatusView{Enabled: true, Status: "revoked", Mode: "block"})
	setSharedLicenseProbe(sharedProbe)
	t.Cleanup(func() { setSharedLicenseProbe(nil) })

	r := gin.New()
	src := ExportSource{SidecarPath: filepath.Join(t.TempDir(), "audit.db")}
	r.GET("/v0/management/contentfilter/export", ExportHTTPHandler(src))

	req := httptest.NewRequest(http.MethodGet, "/v0/management/contentfilter/export?format=csv", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 under invalid license, got %d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-License-Status") != "blocked" {
		t.Fatalf("expected X-License-Status blocked, got %q", rec.Header().Get("X-License-Status"))
	}
	if !strings.Contains(rec.Body.String(), "requires an enterprise license") {
		t.Fatalf("expected clear license-required message, got %s", rec.Body.String())
	}
}
