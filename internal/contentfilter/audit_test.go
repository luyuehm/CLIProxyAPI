package contentfilter

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// TestAuditEnqueueAndSidecarWrite proves the audit writer enqueues a row
// non-blockingly and the sidecar fallback persists it to a local SQLite
// file with the KEEPER schema. The docker-cp path is disabled here so the
// row is forced into the local sidecar; the live KEEPER integration test
// exercises the docker-cp path against the real container.
func TestAuditEnqueueAndSidecarWrite(t *testing.T) {
	sidecar := filepath.Join(t.TempDir(), "audit.db")
	a := NewAudit(AuditEnv{
		HostDBPath:    "/nonexistent/does/not/exist", // force sidecar
		SidecarPath:   sidecar,
		QueueSize:     16,
		WriteTimeout:  2 * time.Second,
		ContainerName: "", // already empty but withDefaults fills it; we patch below
	})
	// Force-disable the docker-cp path so the sidecar is the only writer.
	a.env.ContainerName = ""
	defer a.Close()

	res := a.Enqueue(AuditRow{
		RuleID:          42,
		RuleName:        "test-rule",
		FilterType:      "inbound",
		MatchCount:      2,
		Matches:         "绝密文件,商业机密",
		Action:          "mask",
		Model:           "gpt-5",
		ClientIP:        "127.0.0.1",
		UserID:          "auth:abc",
		RawPreview:      "原文本含绝密文件与商业机密",
		FilteredPreview: "原文本含****与****",
		CreatedAt:       time.Now().UTC(),
	})
	if !res.Enqueued {
		t.Fatalf("expected enqueued, got %+v", res)
	}

	// Wait for the worker to drain.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, _, wr, _, _ := a.Stats()
		if wr >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	enq, drop, wr, fail, _ := a.Stats()
	if wr < 1 {
		t.Fatalf("audit writer did not flush: enq=%d drop=%d wr=%d fail=%d", enq, drop, wr, fail)
	}
	_ = enq
	_ = drop
	_ = fail

	// Read the row back from the sidecar to confirm the schema/contents.
	// Use the "file:" prefix so modernc.org/sqlite treats the DSN as a path
	// rather than a connection name (this matters for cache flushing).
	db, err := sql.Open("sqlite", "file:"+sidecar+"?mode=ro")
	if err != nil {
		t.Fatalf("open sidecar: %v", err)
	}
	defer db.Close()
	row := db.QueryRow(
		`SELECT rule_id, rule_name, filter_type, match_count, matches, action, model, client_ip, user_id
		   FROM content_filter_logs ORDER BY id DESC LIMIT 1`)
	var (
		rid     int64
		rname   string
		ftype   string
		mcount  int
		matches sql.NullString
		action  string
		model   sql.NullString
		ip      sql.NullString
		uid     sql.NullString
	)
	if err := row.Scan(&rid, &rname, &ftype, &mcount, &matches, &action, &model, &ip, &uid); err != nil {
		t.Fatalf("scan row: %v", err)
	}
	if rid != 42 || rname != "test-rule" || ftype != "inbound" || mcount != 2 {
		t.Fatalf("row contents wrong: rid=%d name=%q ftype=%q count=%d", rid, rname, ftype, mcount)
	}
	if !matches.Valid || matches.String == "" {
		t.Fatalf("matches column should be set, got %#v", matches)
	}
}

// TestAuditEnqueueOverflowDrops proves the queue drops rows (rather than
// blocking) when full. This keeps the request hot path immune to audit
// backpressure.
func TestAuditEnqueueOverflowDrops(t *testing.T) {
	a := NewAudit(AuditEnv{
		HostDBPath:   "/nonexistent/does/not/exist",
		SidecarPath:  filepath.Join(t.TempDir(), "audit.db"),
		QueueSize:    2,
		WriteTimeout: 2 * time.Second,
	})
	// Fill the queue with slow sidecar writes so the worker is busy.
	for i := 0; i < 100; i++ {
		a.Enqueue(AuditRow{RuleID: int64(i), RuleName: "flood"})
	}
	enq, drop, _, _, _ := a.Stats()
	if enq+drop != 100 {
		t.Fatalf("enq+drop = %d, want 100", enq+drop)
	}
	// At least one row should be dropped (queue size 2 vs 100 enqueues).
	if drop == 0 {
		t.Fatalf("expected at least one drop with queue=2 and 100 enqueues, got %d", drop)
	}
	a.Close()
}

// TestTruncatePreview covers the preview cap helper.
func TestTruncatePreview(t *testing.T) {
	if got := truncatePreview("hello", 100); got != "hello" {
		t.Fatalf("short string: %q", got)
	}
	if got := truncatePreview("hello world", 5); got != "hello...(truncated)" {
		t.Fatalf("truncated: %q", got)
	}
}

// TestAuditHTTPWriteBatched exercises the RIC-442 HTTP ingest channel end-to-
// end with a fake KEEPER server. The handler validates the request shape
// (JSON body, X-CPA-Management-Key header, URL path) and returns 200 so the
// writer commits the batch in one go.
func TestAuditHTTPWriteBatched(t *testing.T) {
	var (
		gotPath        string
		gotKey         string
		gotRequestHdr  string
		gotBodyLogs    int
		gotBodyFilter  string
		gotBodyAction  string
		gotBodyCreated string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("X-CPA-Management-Key")
		gotRequestHdr = r.Header.Get("X-CPA-Usage-Keeper-Request")
		var body auditHTTPRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		gotBodyLogs = len(body.Logs)
		if gotBodyLogs > 0 {
			gotBodyFilter = body.Logs[0].FilterType
			gotBodyAction = body.Logs[0].Action
			gotBodyCreated = body.Logs[0].CreatedAt
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ingested":` + strconv.Itoa(gotBodyLogs) + `}`))
	}))
	defer srv.Close()

	a := NewAudit(AuditEnv{
		SidecarPath:         filepath.Join(t.TempDir(), "audit.db"),
		QueueSize:           8,
		WriteTimeout:        2 * time.Second,
		BatchSize:           4,
		BatchInterval:       30 * time.Millisecond,
		KEEPERAuditURL:      srv.URL,
		KEEPERManagementKey: "test-key",
	})
	// 不显式给 HostDBPath / ContainerName —— 默认应空、走 HTTP。
	if a.env.HostDBPath != "" {
		t.Fatalf("HostDBPath should be empty by default post-RIC-442, got %q", a.env.HostDBPath)
	}
	if a.env.ContainerName != "" {
		t.Fatalf("ContainerName should be empty by default post-RIC-442, got %q", a.env.ContainerName)
	}
	defer a.Close()

	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	a.Enqueue(AuditRow{
		RuleID:     7,
		RuleName:   "ric442",
		FilterType: "inbound",
		MatchCount: 1,
		Matches:    `["a"]`,
		Action:     "mask",
		Model:      "gpt-5",
		ClientIP:   "127.0.0.1",
		UserID:     "u-1",
		CreatedAt:  now,
	})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, _, wr, _, _ := a.Stats()
		if wr >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, _, wr, _, _ := a.Stats()
	if wr < 1 {
		t.Fatalf("expected at least 1 batched write, got %d", wr)
	}
	if gotPath != "/api/v1/contentfilter/logs/ingest" {
		t.Errorf("ingest path = %q, want /api/v1/contentfilter/logs/ingest", gotPath)
	}
	if gotKey != "test-key" {
		t.Errorf("management key header = %q, want test-key", gotKey)
	}
	if gotRequestHdr != "fetch" {
		t.Errorf("X-CPA-Usage-Keeper-Request = %q, want fetch", gotRequestHdr)
	}
	if gotBodyLogs != 1 {
		t.Errorf("ingested logs = %d, want 1", gotBodyLogs)
	}
	if gotBodyFilter != "inbound" || gotBodyAction != "mask" {
		t.Errorf("body fields wrong: filter=%q action=%q", gotBodyFilter, gotBodyAction)
	}
	if gotBodyCreated != now.Format(time.RFC3339) {
		t.Errorf("body created_at = %q, want %q", gotBodyCreated, now.Format(time.RFC3339))
	}
}

// TestAuditHTTPRetriesSidecar confirms that when the KEEPER HTTP channel
// returns 5xx, the worker falls back to the sidecar so no row is lost
// (RIC-442: 写入通道降级时仍有本地 buffer)。
func TestAuditHTTPRetriesSidecar(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "keeper down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	sidecar := filepath.Join(t.TempDir(), "audit.db")
	a := NewAudit(AuditEnv{
		SidecarPath:         sidecar,
		QueueSize:           4,
		WriteTimeout:        1 * time.Second,
		KEEPERAuditURL:      srv.URL,
		KEEPERManagementKey: "k",
	})
	defer a.Close()
	a.Enqueue(AuditRow{RuleID: 1, RuleName: "fallback", CreatedAt: time.Now().UTC()})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, _, wr, _, _ := a.Stats()
		if wr >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, _, wr, _, _ := a.Stats()
	if wr < 1 {
		t.Fatalf("expected sidecar write after HTTP failure, got wr=%d", wr)
	}
	db, err := sql.Open("sqlite", "file:"+sidecar+"?mode=ro")
	if err != nil {
		t.Fatalf("open sidecar: %v", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM content_filter_logs").Scan(&n); err != nil {
		t.Fatalf("count sidecar: %v", err)
	}
	if n != 1 {
		t.Fatalf("sidecar rows = %d, want 1", n)
	}
}
