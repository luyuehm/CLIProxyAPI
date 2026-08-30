package contentfilter

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// TestLiveKeeperIntegration verifies the syncer can pull rules from the real
// KEEPER container and the engine masks real content. It is opt-in because it
// requires Docker + the KEEPER container; run with:
//
//	CPA_CONTENT_FILTER_LIVE_TEST=1 go test ./internal/contentfilter/ -run TestLiveKeeperIntegration -v
func TestLiveKeeperIntegration(t *testing.T) {
	if os.Getenv("CPA_CONTENT_FILTER_LIVE_TEST") != "1" {
		t.Skip("live KEEPER integration test skipped (set CPA_CONTENT_FILTER_LIVE_TEST=1)")
	}

	opts := DefaultSyncerOptions()
	// Force docker-cp mode by clearing the host path (not present on macOS).
	opts.HostDBPath = ""
	opts.ContainerName = "enterprise-keeper-cpa-usage-keeper-1"
	opts.ContainerDBPath = "/data/app.db"

	s, err := NewSyncer(opts)
	if err != nil {
		t.Fatalf("NewSyncer against live KEEPER: %v", err)
	}
	s.Start()
	defer s.Stop()

	rules := s.Rules()
	if len(rules) < 3 {
		t.Fatalf("live KEEPER rules = %d, want >= 3", len(rules))
	}
	t.Logf("loaded %d live rules:", len(rules))
	for _, r := range rules {
		t.Logf("  id=%d name=%q scenario=%s words=%v pii=%v",
			r.ID, r.Name, r.Scenario, r.SensitiveWords, r.PIITypes)
	}

	e := NewEngine(true)

	// Acceptance #3: inbound sensitive word "绝密文件" -> replaced with mask.
	res := e.Apply(rules, "该文件属于绝密文件，请勿外传", true, "")
	if !res.Changed || strings.Contains(res.Text, "绝密文件") {
		t.Fatalf("inbound sensitive word not masked: %q", res.Text)
	}
	t.Logf("inbound sensitive word -> %q", res.Text)

	// Acceptance #4: outbound PII phone partial masked (138****1234 style).
	phoneRes := e.Apply(rules, "联系电话：13800138000，请查收", false, "")
	if !phoneRes.Changed {
		t.Fatalf("outbound phone not masked: %q", phoneRes.Text)
	}
	t.Logf("outbound phone -> %q", phoneRes.Text)

	// Acceptance #4b: ID card partial masked.
	idRes := e.Apply(rules, "身份证 11010519491231002X 已登记", false, "")
	if !idRes.Changed {
		t.Fatalf("outbound id card not masked: %q", idRes.Text)
	}
	t.Logf("outbound id card -> %q", idRes.Text)

	// Stale flag must be false after a successful load.
	if s.Stale() {
		t.Fatalf("syncer marked stale after successful load")
	}
}

// TestLiveAuditWriteBack proves that the audit writer enqueues a row and the
// row lands in KEEPER's content_filter_logs table via the docker-cp path. It
// runs only when CPA_CONTENT_FILTER_LIVE_TEST=1.
//
// The test pulls a snapshot of the KEEPER db before, fires one inbound
// sensitive-word hit through the audit writer, then pulls a snapshot after
// and asserts a new row exists with the expected fields.
func TestLiveAuditWriteBack(t *testing.T) {
	if os.Getenv("CPA_CONTENT_FILTER_LIVE_TEST") != "1" {
		t.Skip("live audit test skipped (set CPA_CONTENT_FILTER_LIVE_TEST=1)")
	}
	container := "enterprise-keeper-cpa-usage-keeper-1"
	dbPath := "/data/app.db"

	before := countLogs(t, container, dbPath)
	t.Logf("KEEPER content_filter_logs rows before: %d", before)

	audit := NewAudit(AuditEnv{
		HostDBPath:     "", // not present on macOS
		ContainerName:  container,
		ContainerDBPath: dbPath,
		DockerCmd:      "docker",
		QueueSize:      16,
		WriteTimeout:   10 * time.Second,
	})
	defer audit.Close()

	res := audit.Enqueue(AuditRow{
		RuleID:          9999,
		RuleName:        "live-test-rule",
		FilterType:      "inbound",
		MatchCount:      1,
		Matches:         "绝密文件",
		Action:          "mask",
		Model:           "gpt-5",
		ClientIP:        "127.0.0.1",
		UserID:          "auth:live",
		RawPreview:      "test 绝密文件 [live]",
		FilteredPreview: "test **** [live]",
		CreatedAt:       time.Now().UTC(),
	})
	if !res.Enqueued {
		t.Fatalf("expected enqueued, got %+v", res)
	}

	// Wait for the worker to flush (docker cp round-trip takes ~1s).
	deadline := time.Now().Add(15 * time.Second)
	var after int
	for time.Now().Before(deadline) {
		after = countLogs(t, container, dbPath)
		if after > before {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if after <= before {
		t.Fatalf("audit row did not land in KEEPER: before=%d after=%d", before, after)
	}
	t.Logf("KEEPER content_filter_logs rows after: %d (delta=%d)", after, after-before)

	// Verify the new row has the expected fields.
	db := pullKEEPER(t, container, dbPath)
	defer db.Close()
	row := db.QueryRow(
		`SELECT rule_id, rule_name, filter_type, match_count, matches, action, model, client_ip, raw_preview, filtered_preview
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
		raw     sql.NullString
		filt    sql.NullString
	)
	if err := row.Scan(&rid, &rname, &ftype, &mcount, &matches, &action, &model, &ip, &raw, &filt); err != nil {
		t.Fatalf("scan latest row: %v", err)
	}
	if rid != 9999 || rname != "live-test-rule" || ftype != "inbound" || mcount != 1 {
		t.Fatalf("row fields wrong: rid=%d name=%q ftype=%q count=%d", rid, rname, ftype, mcount)
	}
	if !matches.Valid || !strings.Contains(matches.String, "绝密文件") {
		t.Fatalf("matches wrong: %#v", matches)
	}
	if !raw.Valid || !strings.Contains(raw.String, "绝密文件") {
		t.Fatalf("raw_preview missing sensitive word: %#v", raw)
	}
	if !filt.Valid || strings.Contains(filt.String, "绝密文件") {
		t.Fatalf("filtered_preview should not contain the original: %#v", filt)
	}
	t.Logf("latest KEEPER audit row: id/rule_id=%d name=%q ftype=%q raw=%q filtered=%q",
		rid, rname, ftype, raw.String, filt.String)
}

// countLogs returns the number of rows currently in KEEPER's
// content_filter_logs table.
func countLogs(t *testing.T, container, dbPath string) int {
	t.Helper()
	db := pullKEEPER(t, container, dbPath)
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM content_filter_logs").Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return n
}

// pullKEEPER copies the KEEPER app.db into a temp file and opens it read-only.
func pullKEEPER(t *testing.T, container, dbPath string) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	local := filepath.Join(dir, "app.db")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "cp", container+":"+dbPath, local)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("docker cp: %v: %s", err, strings.TrimSpace(string(out)))
	}
	db, err := sql.Open("sqlite", "file:"+local+"?mode=ro")
	if err != nil {
		t.Fatalf("open pulled db: %v", err)
	}
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		_ = db.Close()
		t.Fatalf("busy_timeout: %v", err)
	}
	return db
}
