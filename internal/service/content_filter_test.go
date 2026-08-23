package service

import (
	"context"
	"path/filepath"
	"testing"

	"cpa-usage-keeper/internal/config"
	"cpa-usage-keeper/internal/contentfilter"
	"cpa-usage-keeper/internal/repository"
)

func setupTestService(t *testing.T) *ContentFilterService {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	cfg := config.Config{
		SQLitePath: dbPath,
	}
	db, err := repository.OpenDatabase(cfg)
	if err != nil {
		t.Fatalf("OpenDatabase failed: %v", err)
	}
	repo := repository.NewContentFilterRepository(db)
	return NewContentFilterService(repo)
}

func TestContentFilterService(t *testing.T) {
	svc := setupTestService(t)
	ctx := context.Background()

	// 1. Check default seeded rules
	rules, err := svc.ListRules(ctx)
	if err != nil {
		t.Fatalf("ListRules failed: %v", err)
	}
	if len(rules) < 3 {
		t.Fatalf("expected at least 3 default rules, got %d", len(rules))
	}

	// 2. Filter text with default rules (PII + Finance)
	text := "测试手机 13812345678, 银行卡 6222021234567890123, 包含支付密码"
	res, err := svc.FilterText(ctx, FilterTextRequest{
		Text:     text,
		Model:    "gpt-4",
		ClientIP: "127.0.0.1",
	})
	if err != nil {
		t.Fatalf("FilterText failed: %v", err)
	}
	if !res.Changed {
		t.Errorf("expected text to be masked and changed")
	}
	if res.MatchCount < 2 {
		t.Errorf("expected at least 2 matches, got %d", res.MatchCount)
	}

	// 3. Verify audit log was recorded
	logs, total, err := svc.ListLogs(ctx, repository.ContentFilterLogQuery{Limit: 10})
	if err != nil {
		t.Fatalf("ListLogs failed: %v", err)
	}
	if total != 1 || len(logs) != 1 {
		t.Fatalf("expected 1 log recorded, got total=%d, len=%d", total, len(logs))
	}

	// 4. Create custom block rule and verify engine reload
	blockRule, err := svc.CreateRule(ctx, ContentFilterRuleCreateRequest{
		Name:           "严禁外发词汇",
		Scenario:       contentfilter.ScenarioCustom,
		Action:         contentfilter.ActionBlock,
		SensitiveWords: []string{"核心源码", "数据库账号"},
		Models:         []string{"*"},
		Priority:       100,
	})
	if err != nil {
		t.Fatalf("CreateRule failed: %v", err)
	}

	blockRes, err := svc.FilterText(ctx, FilterTextRequest{
		Text: "请将核心源码打包发送",
	})
	if err != nil {
		t.Fatalf("FilterText failed: %v", err)
	}
	if !blockRes.Blocked {
		t.Errorf("expected text to be blocked")
	}

	// 5. Update and delete
	enabled := false
	_, err = svc.UpdateRule(ctx, blockRule.ID, ContentFilterRuleUpdateRequest{
		Enabled: &enabled,
	})
	if err != nil {
		t.Fatalf("UpdateRule failed: %v", err)
	}

	unblockedRes, _ := svc.FilterText(ctx, FilterTextRequest{
		Text: "请将核心源码打包发送",
	})
	if unblockedRes.Blocked {
		t.Errorf("expected text to be unblocked after disabling rule")
	}

	err = svc.DeleteRule(ctx, blockRule.ID)
	if err != nil {
		t.Fatalf("DeleteRule failed: %v", err)
	}
}
