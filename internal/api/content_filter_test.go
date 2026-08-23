package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"cpa-usage-keeper/internal/auth"
	"cpa-usage-keeper/internal/config"
	"cpa-usage-keeper/internal/contentfilter"
	"cpa-usage-keeper/internal/repository"
	"cpa-usage-keeper/internal/service"
	"github.com/gin-gonic/gin"
)

func setupTestAPIRouter(t *testing.T) (*gin.Engine, service.ContentFilterProvider) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	cfg := config.Config{
		SQLitePath: dbPath,
	}
	db, err := repository.OpenDatabase(cfg)
	if err != nil {
		t.Fatalf("OpenDatabase failed: %v", err)
	}

	contentFilterRepo := repository.NewContentFilterRepository(db)
	contentFilterService := service.NewContentFilterService(contentFilterRepo)

	authConfig := AuthConfig{
		Enabled: false,
	}
	sessionManager := auth.NewSessionManager(3600)
	authHandler := NewAuthHandler(authConfig, sessionManager)

	router := NewRouter(
		nil,
		nil,
		nil,
		nil,
		authConfig,
		authHandler,
		"",
		OptionalProviders{
			ContentFilter: contentFilterService,
		},
	)

	return router, contentFilterService
}

func TestContentFilterAPIRoutes(t *testing.T) {
	router, _ := setupTestAPIRouter(t)

	// 1. GET /api/v1/contentfilter/rules
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/contentfilter/rules", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var listResp contentFilterRuleListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("failed to unmarshal rules: %v", err)
	}
	if len(listResp.Rules) < 3 {
		t.Fatalf("expected at least 3 default rules, got %d", len(listResp.Rules))
	}

	// 2. POST /api/v1/contentfilter/rules
	createBody := service.ContentFilterRuleCreateRequest{
		Name:           "API测试规则",
		Description:    "测试描述",
		Scenario:       contentfilter.ScenarioFinance,
		Action:         contentfilter.ActionBlock,
		SensitiveWords: []string{"内部账号", "特权密码"},
		PIITypes:       []string{contentfilter.PIITypePhone},
		WhiteList:      []string{"admin"},
		Models:         []string{"*"},
		Priority:       99,
	}
	bodyBytes, _ := json.Marshal(createBody)
	req, _ = http.NewRequest(http.MethodPost, "/api/v1/contentfilter/rules", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var createdRule contentFilterRuleResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &createdRule); err != nil {
		t.Fatalf("failed to unmarshal created rule: %v", err)
	}
	if createdRule.ID == 0 || createdRule.Name != "API测试规则" {
		t.Fatalf("unexpected created rule: %+v", createdRule)
	}

	// 3. GET /api/v1/contentfilter/rules/:id
	req, _ = http.NewRequest(http.MethodGet, "/api/v1/contentfilter/rules/"+strconvFormat(createdRule.ID), nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// 4. PUT /api/v1/contentfilter/rules/:id
	updateBody := service.ContentFilterRuleUpdateRequest{
		Name:   "API更新后规则",
		Action: contentfilter.ActionRedact,
	}
	updateBytes, _ := json.Marshal(updateBody)
	req, _ = http.NewRequest(http.MethodPut, "/api/v1/contentfilter/rules/"+strconvFormat(createdRule.ID), bytes.NewReader(updateBytes))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// 5. POST /api/v1/contentfilter/test
	testBody := service.FilterTextRequest{
		Text:  "请查询手机 13800138000 和 内部账号",
		Model: "gpt-4o",
	}
	testBytes, _ := json.Marshal(testBody)
	req, _ = http.NewRequest(http.MethodPost, "/api/v1/contentfilter/test", bytes.NewReader(testBytes))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var testResp service.FilterTextResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &testResp); err != nil {
		t.Fatalf("failed to unmarshal test response: %v", err)
	}
	if !testResp.Changed {
		t.Errorf("expected test response text to be changed")
	}

	// 6. GET /api/v1/contentfilter/logs
	req, _ = http.NewRequest(http.MethodGet, "/api/v1/contentfilter/logs?limit=10", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var logResp contentFilterLogListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &logResp); err != nil {
		t.Fatalf("failed to unmarshal logs: %v", err)
	}
	if logResp.Total < 1 || len(logResp.Logs) < 1 {
		t.Fatalf("expected at least 1 log, got total=%d", logResp.Total)
	}

	// 7. DELETE /api/v1/contentfilter/rules/:id
	req, _ = http.NewRequest(http.MethodDelete, "/api/v1/contentfilter/rules/"+strconvFormat(createdRule.ID), nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func strconvFormat(n int64) string {
	return strconv.FormatInt(n, 10)
}
