package repository

import (
	"context"
	"path/filepath"
	"testing"

	"cpa-usage-keeper/internal/config"
	"cpa-usage-keeper/internal/contentfilter"
	"cpa-usage-keeper/internal/entities"
)

func setupTestDB(t *testing.T) *ContentFilterRepository {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	cfg := config.Config{
		SQLitePath: dbPath,
	}
	db, err := OpenDatabase(cfg)
	if err != nil {
		t.Fatalf("OpenDatabase failed: %v", err)
	}
	return NewContentFilterRepository(db)
}

func TestContentFilterRepositoryCRUD(t *testing.T) {
	repo := setupTestDB(t)
	ctx := context.Background()

	// 1. Seed defaults
	if err := repo.SeedDefaultRulesIfEmpty(ctx); err != nil {
		t.Fatalf("SeedDefaultRulesIfEmpty failed: %v", err)
	}

	rules, err := repo.ListRules(ctx)
	if err != nil {
		t.Fatalf("ListRules failed: %v", err)
	}
	if len(rules) != 3 {
		t.Fatalf("expected 3 default rules, got %d", len(rules))
	}

	// 2. Create custom rule
	newRule := &entities.ContentFilterRule{
		Name:           "自定义阻断规则",
		Description:    "测试阻断",
		Enabled:        true,
		Scenario:       contentfilter.ScenarioCustom,
		Action:         contentfilter.ActionBlock,
		SensitiveWords: "秘密1,秘密2",
		PIITypes:       "phone",
		WhiteList:      "root",
		Models:         "gpt-4",
		Priority:       50,
	}
	if err := repo.CreateRule(ctx, newRule); err != nil {
		t.Fatalf("CreateRule failed: %v", err)
	}
	if newRule.ID == 0 {
		t.Fatal("expected non-zero ID for created rule")
	}

	// 3. Get rule
	gotRule, err := repo.GetRuleByID(ctx, newRule.ID)
	if err != nil {
		t.Fatalf("GetRuleByID failed: %v", err)
	}
	if gotRule.Name != "自定义阻断规则" {
		t.Errorf("expected name '自定义阻断规则', got '%s'", gotRule.Name)
	}

	// 4. Update rule
	gotRule.Name = "更新后的规则"
	gotRule.Action = contentfilter.ActionRedact
	if err := repo.UpdateRule(ctx, gotRule); err != nil {
		t.Fatalf("UpdateRule failed: %v", err)
	}

	updatedRule, _ := repo.GetRuleByID(ctx, newRule.ID)
	if updatedRule.Name != "更新后的规则" || updatedRule.Action != contentfilter.ActionRedact {
		t.Errorf("rule update did not persist correctly: %+v", updatedRule)
	}

	// 5. Create and list logs
	log := &entities.ContentFilterLog{
		RuleID:          updatedRule.ID,
		RuleName:        updatedRule.Name,
		FilterType:      "pii",
		MatchCount:      1,
		Matches:         "phone",
		Action:          contentfilter.ActionRedact,
		Model:           "gpt-4",
		ClientIP:        "127.0.0.1",
		RawPreview:      "手机号 13812345678",
		FilteredPreview: "手机号 [REDACTED_PHONE]",
	}
	if err := repo.CreateLog(ctx, log); err != nil {
		t.Fatalf("CreateLog failed: %v", err)
	}

	logs, total, err := repo.ListLogs(ctx, ContentFilterLogQuery{Limit: 10})
	if err != nil {
		t.Fatalf("ListLogs failed: %v", err)
	}
	if total != 1 || len(logs) != 1 {
		t.Fatalf("expected 1 log, got total=%d, len=%d", total, len(logs))
	}

	// 6. Delete rule
	if err := repo.DeleteRule(ctx, newRule.ID); err != nil {
		t.Fatalf("DeleteRule failed: %v", err)
	}
	_, err = repo.GetRuleByID(ctx, newRule.ID)
	if err != ErrContentFilterRuleNotFound {
		t.Errorf("expected ErrContentFilterRuleNotFound after delete, got %v", err)
	}
}
