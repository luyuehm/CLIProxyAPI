package contentfilter

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// --- RIC-440 false-positive regression tests ---

func TestVerifierRejectsPhonePlaceholders(t *testing.T) {
	for _, p := range []string{
		"13800138000", "13800138001", "13000000000", "13900000000",
		"11111111111", "12345678901", "01234567890",
	} {
		if verifyPhone(p) {
			t.Fatalf("verifyPhone(%q) = true, want false (placeholder)", p)
		}
	}
}

func TestVerifierAcceptsRealPhones(t *testing.T) {
	for _, p := range []string{
		"13812345678", "15698765432", "17712344321", "19987654321",
	} {
		if !verifyPhone(p) {
			t.Fatalf("verifyPhone(%q) = false, want true", p)
		}
	}
}

func TestVerifyIDCardCheckDigit(t *testing.T) {
	if !verifyIDCard("11010519491231002X") {
		t.Fatalf("11010519491231002X should pass GB 11643 check")
	}
	if verifyIDCard("11010519491231003X") {
		t.Fatalf("mutated id card should fail check digit")
	}
	if verifyIDCard("11111111111111111X") {
		t.Fatalf("all-same id card should fail")
	}
}

func TestVerifyBankCardLuhn(t *testing.T) {
	if !verifyBankCard("4111111111111111") {
		t.Fatalf("4111... should pass Luhn")
	}
	if verifyBankCard("4111111111111112") {
		t.Fatalf("off-by-one should fail Luhn")
	}
	if !verifyBankCard("9342253601060259203") {
		t.Fatalf("19-digit Luhn-valid should pass")
	}
	if verifyBankCard("0000000000000000") {
		t.Fatalf("all-zero should fail")
	}
}

func TestVerifyEmailSyntheticReject(t *testing.T) {
	if verifyEmail("12345@x.cn") {
		t.Fatalf("all-digit local part should reject")
	}
	if !verifyEmail("alice@example.com") {
		t.Fatalf("real email should pass")
	}
	if !verifyEmail("alice.smith@acme-corp.com") {
		t.Fatalf("dashed-domain email should pass")
	}
}

func TestEngineNoMaskPlaceholderPhone(t *testing.T) {
	e := NewEngine(true)
	rules := []*Rule{newTestRule(1, "r1", nil, []PIIType{PIIPhone})}
	res := e.Apply(rules, "联系电话 13800138000 请查收", true, "")
	if res.Changed {
		t.Fatalf("placeholder phone should NOT be masked: got %q", res.Text)
	}
}

func TestEngineNoMaskSyntheticEmail(t *testing.T) {
	e := NewEngine(true)
	rules := []*Rule{newTestRule(1, "r1", nil, []PIIType{PIIEmail})}
	res := e.Apply(rules, "see 12345@x.cn for details", true, "")
	if res.Changed {
		t.Fatalf("synthetic email should NOT be masked: got %q", res.Text)
	}
}

func TestEngineMasksRealPhone(t *testing.T) {
	e := NewEngine(true)
	rules := []*Rule{newTestRule(1, "r1", nil, []PIIType{PIIPhone})}
	res := e.Apply(rules, "联系电话 13812345678 请查收", true, "")
	if !res.Changed {
		t.Fatalf("real phone must be masked")
	}
	if strings.Contains(res.Text, "13812345678") {
		t.Fatalf("phone leaked: %q", res.Text)
	}
}

func TestEngineContextExclusion(t *testing.T) {
	e := NewEngine(true)
	r := newTestRule(1, "r1", nil, []PIIType{PIIPhone})
	r.ContextExcludes = []string{"订单号", "commit", "sha256"}
	rules := []*Rule{r}

	res := e.Apply(rules, "订单号 13812345678", true, "")
	if res.Changed {
		t.Fatalf("订单号 context should exempt phone: got %q", res.Text)
	}
	res = e.Apply(rules, "git commit 13812345678 message", true, "")
	if res.Changed {
		t.Fatalf("commit context should exempt phone: got %q", res.Text)
	}
	res = e.Apply(rules, "电话 13812345678", true, "")
	if !res.Changed {
		t.Fatalf("bare phone should still mask")
	}
}

func TestEngineMinMatchLen(t *testing.T) {
	e := NewEngine(true)
	r := newTestRule(1, "r1", nil, []PIIType{PIIBankCard})
	r.MinMatchLen = 13
	rules := []*Rule{r}
	res := e.Apply(rules, "seq 411111111111", true, "")
	if res.Changed {
		t.Fatalf("12-digit run should NOT match with MinMatchLen=13: got %q", res.Text)
	}
}

func TestWhitelistModes(t *testing.T) {
	cases := []struct {
		name  string
		mode  WhitelistMode
		entry string
		value string
		want  bool
	}{
		{"exact_match", WhitelistModeExact, "13812345678", "13812345678", true},
		{"exact_no", WhitelistModeExact, "13812345678", "13899999999", false},
		{"prefix", WhitelistModePrefix, "138", "13812345678", true},
		{"prefix_no", WhitelistModePrefix, "138", "13912345678", false},
		{"suffix", WhitelistModeSuffix, "5678", "13812345678", true},
		{"contains", WhitelistModeContains, "2345", "13812345678", true},
		{"regex", WhitelistModeRegex, `^138[0-9]{8}$`, "13812345678", true},
		{"regex_no", WhitelistModeRegex, `^138[0-9]{8}$`, "13912345678", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newTestRule(1, "r1", nil, nil)
			r.WhitelistMode = c.mode
			r.WhiteList = []string{c.entry}
			if got := r.isWhitelisted(c.value); got != c.want {
				t.Fatalf("mode=%s entry=%q value=%q got=%v want=%v",
					c.mode, c.entry, c.value, got, c.want)
			}
		})
	}
}

func TestActionAllowSkipsRule(t *testing.T) {
	e := NewEngine(true)
	r := newTestRule(1, "r1", []string{"绝密文件"}, nil)
	r.Action = ActionAllow
	res := e.Apply([]*Rule{r}, "绝密文件", true, "")
	if res.Changed {
		t.Fatalf("action=allow must not mask: got %q", res.Text)
	}
}

// --- RIC-440 audit / export / syncer tests ---

func TestAuditBatchedInsert(t *testing.T) {
	sidecar := filepath.Join(t.TempDir(), "audit.db")
	a := NewAudit(AuditEnv{
		HostDBPath:    "/nonexistent/does/not/exist",
		SidecarPath:   sidecar,
		QueueSize:     32,
		WriteTimeout:  2 * time.Second,
		BatchSize:     8,
		BatchInterval: 50 * time.Millisecond,
	})
	a.env.ContainerName = ""
	defer a.Close()

	const N = 20
	for i := 0; i < N; i++ {
		a.Enqueue(AuditRow{
			RuleID:     int64(i),
			RuleName:   "batch-test",
			FilterType: "inbound",
			MatchCount: 1,
			Matches:    MarshalMatchesJSON([]string{"foo"}),
			Action:     "mask",
			CreatedAt:  time.Now().UTC(),
		})
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, _, wr, _, _ := a.Stats()
		if wr >= N {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, _, wr, _, batches := a.Stats()
	if wr < N {
		t.Fatalf("expected %d rows, got %d", N, wr)
	}
	if batches < 2 {
		t.Fatalf("expected batching to fire at least 2 batches, got %d", batches)
	}
	db, err := sql.Open("sqlite", "file:"+sidecar+"?mode=ro")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	row := db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_cflogs_created_at'`)
	var name string
	if err := row.Scan(&name); err != nil {
		t.Fatalf("expected idx_cflogs_created_at index, got %v", err)
	}
}

func TestAuditJSONMatches(t *testing.T) {
	if got := MarshalMatchesJSON([]string{"a", "b", "c"}); got != `["a","b","c"]` {
		t.Fatalf("MarshalMatchesJSON = %q", got)
	}
	if got := MarshalMatchesJSON(nil); got != "[]" {
		t.Fatalf("MarshalMatchesJSON(nil) = %q, want []", got)
	}
}

func TestSyncerHashDiffNoOp(t *testing.T) {
	path := createKeeperTestDB(t, [][]interface{}{
		{"r1", "", 1, "general", "mask", "绝密文件", "", "", "*", 10},
	})
	opts := SyncerOptions{HostDBPath: path, RefreshInterval: time.Hour}
	s, err := NewSyncer(opts)
	if err != nil {
		t.Fatalf("NewSyncer: %v", err)
	}
	defer s.Close()
	n1, _ := s.Reload()
	if n1 != 1 {
		t.Fatalf("first reload = %d, want 1", n1)
	}
	n2, _ := s.Reload()
	if n2 != 1 {
		t.Fatalf("second reload = %d, want 1", n2)
	}
	h1 := hashRules(s.Rules())
	h2 := hashRules(s.Rules())
	if h1 != h2 {
		t.Fatalf("hashRules not stable: %s vs %s", h1, h2)
	}
}

func TestSyncerMetaTable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE content_filter_rules (
		id integer PRIMARY KEY AUTOINCREMENT,
		name text NOT NULL,
		description text,
		enabled numeric NOT NULL DEFAULT true,
		scenario text NOT NULL DEFAULT 'general',
		action text NOT NULL DEFAULT 'mask',
		sensitive_words text,
		pii_types text,
		white_list text,
		models text,
		priority integer NOT NULL DEFAULT 0,
		created_at datetime,
		updated_at datetime
	)`); err != nil {
		t.Fatalf("create rules table: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE content_filter_rule_meta (
		rule_id integer PRIMARY KEY,
		whitelist_mode text,
		context_excludes text,
		min_match_len integer
	)`); err != nil {
		t.Fatalf("create meta table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO content_filter_rules
		(name, enabled, scenario, action, sensitive_words, pii_types, white_list, models, priority)
		VALUES (?, 1, 'general', 'mask', '', 'phone', 'whitelist_entry', '*', 10)`,
		"phone-rule"); err != nil {
		t.Fatalf("insert rule: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO content_filter_rule_meta
		(rule_id, whitelist_mode, context_excludes, min_match_len)
		VALUES (1, 'prefix', '订单号' || char(10) || 'commit', 11)`); err != nil {
		t.Fatalf("insert meta: %v", err)
	}
	rules, err := readRulesFromDB(path)
	if err != nil {
		t.Fatalf("readRulesFromDB: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("want 1 rule, got %d", len(rules))
	}
	if rules[0].WhitelistMode != WhitelistModePrefix {
		t.Fatalf("WhitelistMode = %q, want prefix", rules[0].WhitelistMode)
	}
	if len(rules[0].ContextExcludes) != 2 {
		t.Fatalf("ContextExcludes = %v, want 2 entries", rules[0].ContextExcludes)
	}
	if rules[0].MinMatchLen != 11 {
		t.Fatalf("MinMatchLen = %d, want 11", rules[0].MinMatchLen)
	}
}

func TestExportLogsRoundTrip(t *testing.T) {
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
			RawPreview: "phone 13812345678", FilteredPreview: "phone 138****5678",
			CreatedAt: now},
		{RuleID: 2, RuleName: "email", FilterType: "outbound", MatchCount: 1,
			Matches: `["alice@example.com"]`, Action: "mask", Model: "gpt-5",
			ClientIP: "10.0.0.2", UserID: "auth:def",
			RawPreview: "alice@example.com", FilteredPreview: "a***@*******.com",
			CreatedAt: now.Add(time.Minute)},
	}
	if err := insertLogBatch(db, rows); err != nil {
		t.Fatalf("insert: %v", err)
	}
	db.Close()

	src := ExportSource{SidecarPath: sidecar}

	t.Run("csv", func(t *testing.T) {
		out := filepath.Join(dir, "out.csv")
		res, err := ExportLogs(src, ExportFilter{}, ExportCSV, out)
		if err != nil {
			t.Fatalf("ExportLogs CSV: %v", err)
		}
		if res.Rows != 2 {
			t.Fatalf("rows = %d, want 2", res.Rows)
		}
		b, _ := os.ReadFile(out)
		body := string(b)
		if !strings.Contains(body, "phone") || !strings.Contains(body, "email") {
			t.Fatalf("csv body missing rules: %q", body)
		}
		if !strings.HasPrefix(body, "id,rule_id,rule_name,") {
			t.Fatalf("csv header missing: %q", body)
		}
	})

	t.Run("jsonl", func(t *testing.T) {
		out := filepath.Join(dir, "out.jsonl")
		res, err := ExportLogs(src, ExportFilter{}, ExportJSONL, out)
		if err != nil {
			t.Fatalf("ExportLogs JSONL: %v", err)
		}
		if res.Rows != 2 {
			t.Fatalf("rows = %d, want 2", res.Rows)
		}
		b, _ := os.ReadFile(out)
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		if len(lines) != 2 {
			t.Fatalf("jsonl lines = %d, want 2", len(lines))
		}
		var rec ExportRecord
		if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
			t.Fatalf("jsonl[0] not valid json: %v", err)
		}
		if rec.RuleName != "phone" {
			t.Fatalf("jsonl[0].RuleName = %q", rec.RuleName)
		}
	})

	t.Run("json", func(t *testing.T) {
		out := filepath.Join(dir, "out.json")
		res, err := ExportLogs(src, ExportFilter{}, ExportJSON, out)
		if err != nil {
			t.Fatalf("ExportLogs JSON: %v", err)
		}
		if res.Rows != 2 {
			t.Fatalf("rows = %d, want 2", res.Rows)
		}
		b, _ := os.ReadFile(out)
		var arr []ExportRecord
		if err := json.Unmarshal(b, &arr); err != nil {
			t.Fatalf("json not valid array: %v", err)
		}
		if len(arr) != 2 {
			t.Fatalf("json arr len = %d, want 2", len(arr))
		}
	})

	t.Run("filter_since", func(t *testing.T) {
		out := filepath.Join(dir, "filtered.jsonl")
		res, err := ExportLogs(src, ExportFilter{Since: now.Add(30 * time.Second)}, ExportJSONL, out)
		if err != nil {
			t.Fatalf("ExportLogs filter: %v", err)
		}
		if res.Rows != 1 {
			t.Fatalf("filtered rows = %d, want 1", res.Rows)
		}
	})
}

func TestAuditPreviewCap(t *testing.T) {
	if auditPreviewMax != 1024 {
		t.Fatalf("auditPreviewMax = %d, want 1024 (1 KiB)", auditPreviewMax)
	}
}
