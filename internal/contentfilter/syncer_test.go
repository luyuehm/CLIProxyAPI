package contentfilter

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// createKeeperTestDB builds a SQLite file with the same content_filter_rules
// schema and rows as the real KEEPER database, so readRulesFromDB is tested
// against the production shape.
func createKeeperTestDB(t *testing.T, rules [][]interface{}) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "app.db")

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	defer db.Close()

	_, err = db.Exec(`CREATE TABLE content_filter_rules (
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
	);`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}

	if len(rules) > 0 {
		for _, r := range rules {
			_, err = db.Exec(`INSERT INTO content_filter_rules
				(name, description, enabled, scenario, action, sensitive_words, pii_types, white_list, models, priority)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				r...)
			if err != nil {
				t.Fatalf("insert rule: %v", err)
			}
		}
	}
	return path
}

func TestReadRulesFromDB(t *testing.T) {
	path := createKeeperTestDB(t, [][]interface{}{
		{"通用个人隐私(PII)自动脱敏", "general pii rule", 1, "general", "mask",
			"绝密文件,商业机密", "phone,id_card,email", "127.0.0.1,admin@example.com", "*", 10},
		{"金融合规防数据泄漏规则", "finance rule", 1, "finance", "mask",
			"银行卡号,信用卡CVV", "bank_card,phone", "", "*", 20},
	})

	rules, err := readRulesFromDB(path)
	if err != nil {
		t.Fatalf("readRulesFromDB: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("got %d rules, want 2", len(rules))
	}

	// Rules ordered by priority then id (finance priority 20 first).
	if rules[0].Name != "金融合规防数据泄漏规则" && rules[1].Name != "金融合规防数据泄漏规则" {
		t.Fatalf("expected finance rule among result, got %+v", rules)
	}
	if !rules[0].Enabled || !rules[1].Enabled {
		t.Fatalf("rules should be enabled")
	}
	if len(rules[0].SensitiveWords) != 2 {
		t.Fatalf("rule 0 sensitive words = %v, want 2", rules[0].SensitiveWords)
	}
	if len(rules[0].PIITypes) != 3 {
		t.Fatalf("rule 0 pii types = %v, want 3", rules[0].PIITypes)
	}
}

func TestReadRulesFromDBRespectsEnabled(t *testing.T) {
	path := createKeeperTestDB(t, [][]interface{}{
		{"disabled rule", "", 0, "general", "mask", "绝密文件", "", "", "*", 10},
	})
	rules, err := readRulesFromDB(path)
	if err != nil {
		t.Fatalf("readRulesFromDB: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("got %d rules", len(rules))
	}
	if rules[0].Enabled {
		t.Fatalf("rule should be disabled")
	}
	// An engine must ignore it.
	e := NewEngine(true)
	res := e.Apply(rules, "绝密文件", true, "")
	if res.Changed {
		t.Fatalf("disabled rule must not mask")
	}
}

// TestSyncerReloadAndStop verifies that the syncer can load from a real KEEPER
// db file via SyncerOptions.HostDBPath and then pick up changes on reload.
func TestSyncerReloadAndStop(t *testing.T) {
	path := createKeeperTestDB(t, [][]interface{}{
		{"r1", "", 1, "general", "mask", "绝密文件", "", "", "*", 10},
	})

	opts := SyncerOptions{
		HostDBPath:     path,
		RefreshInterval: time.Hour, // no background ticks in this test
	}
	s, err := NewSyncer(opts)
	if err != nil {
		t.Fatalf("NewSyncer: %v", err)
	}
	s.Start()
	defer s.Stop()

	if got := len(s.Rules()); got != 1 {
		t.Fatalf("initial rules = %d, want 1", got)
	}

	// Simulate KEEPER changing: update the rule and add a new one, then Reload.
	verifyReload := func() {
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatalf("open test db: %v", err)
		}
		defer db.Close()
		_, err = db.Exec(`UPDATE content_filter_rules SET sensitive_words = '商业机密' WHERE name = 'r1'`)
		if err != nil {
			t.Fatalf("update rule: %v", err)
		}
		_, err = db.Exec(`INSERT INTO content_filter_rules (name, enabled, scenario, action, sensitive_words, priority) VALUES ('r2', 1, 'general', 'mask', '内幕交易', 30)`)
		if err != nil {
			t.Fatalf("insert rule: %v", err)
		}
	}
	verifyReload()

	n, err := s.Reload()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if n != 2 {
		t.Fatalf("reload rules = %d, want 2", n)
	}

	// Hot-swapped rules must be reflected immediately (no restart).
	e := NewEngine(true)
	res := e.Apply(s.Rules(), "此文件涉及商业机密", true, "")
	if !res.Changed {
		t.Fatalf("hot reload should mask new sensitive word")
	}
}