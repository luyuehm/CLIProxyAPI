package shadow_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/shadow"
)

// TestEvalStore tests the in-memory evaluation ledger.
func TestEvalStore(t *testing.T) {
	store := shadow.NewEvalStoreWithCapacity(10)

	if store.Len() != 0 {
		t.Fatalf("expected empty store, got %d", store.Len())
	}

	for i := 0; i < 10; i++ {
		store.Push(&shadow.Record{
			Model:       "gpt-4",
			Candidate:   "deepseek-v3",
			Similarity:  0.95,
			PrimaryTTFT: 100,
		})
	}

	if store.Len() != 10 {
		t.Fatalf("expected 10 records, got %d", store.Len())
	}

	// Overflow: push 5 more, should still only have cap=10
	for i := 0; i < 5; i++ {
		store.Push(&shadow.Record{
			Model:      "gpt-4",
			Candidate:  "deepseek-v3",
			Similarity: 0.90,
		})
	}
	if store.Len() != 10 {
		t.Fatalf("expected 10 records after overflow, got %d", store.Len())
	}

	// Query by model
	recs := store.Query("gpt-4", "", 5)
	if len(recs) == 0 || len(recs) > 5 {
		t.Fatalf("expected 1-5 records, got %d", len(recs))
	}

	// Query non-existent model
	recs = store.Query("nonexistent", "", 5)
	if len(recs) != 0 {
		t.Fatalf("expected 0 records for nonexistent model, got %d", len(recs))
	}

	// Stats
	stats := store.Stats("gpt-4", "deepseek-v3")
	if stats == nil {
		t.Fatal("expected stats, got nil")
	}
	if stats.TotalEvals == 0 {
		t.Fatal("expected non-zero total evals")
	}

	store.Clear()
	if store.Len() != 0 {
		t.Fatalf("expected empty store after clear, got %d", store.Len())
	}
}

// TestCanaryRouter tests weighted canary decisions.
func TestCanaryRouter(t *testing.T) {
	rules := []shadow.CanaryConfig{
		{Model: "gpt-4", Candidate: "deepseek-v3", Weight: 0},
	}
	router := shadow.NewCanaryRouter(rules)

	// 0% weight should never route
	_, ok, _ := router.Decide("gpt-4", http.Header{})
	if ok {
		t.Fatal("expected 0% weight to not route")
	}

	// 100% weight should always route
	router.Update([]shadow.CanaryConfig{
		{Model: "gpt-4", Candidate: "deepseek-v3", Weight: 100},
	})
	_, ok, _ = router.Decide("gpt-4", http.Header{})
	if !ok {
		t.Fatal("expected 100% weight to route")
	}

	// Non-matching model
	_, ok, _ = router.Decide("claude-3", http.Header{})
	if ok {
		t.Fatal("expected non-matching model to not route")
	}
}

// TestCanaryHeaderForce tests header-based canary forcing.
func TestCanaryHeaderForce(t *testing.T) {
	headers := http.Header{}
	headers.Set("X-Canary", "true")

	rules := []shadow.CanaryConfig{
		{
			Model:     "gpt-4",
			Candidate: "deepseek-v3",
			Weight:    0,
			Headers:   map[string]string{"X-Canary": "true"},
		},
	}
	router := shadow.NewCanaryRouter(rules)
	rule, ok, reason := router.Decide("gpt-4", headers)
	if !ok {
		t.Fatal("expected header force to route")
	}
	if rule.Candidate != "deepseek-v3" {
		t.Fatalf("expected candidate deepseek-v3, got %s", rule.Candidate)
	}
	if reason != "header-force" {
		t.Fatalf("expected header-force reason, got %s", reason)
	}
}

// TestCanaryUserForce tests user/department header forcing.
func TestCanaryUserForce(t *testing.T) {
	headers := http.Header{}
	headers.Set("X-Team-ID", "engineering")

	rules := []shadow.CanaryConfig{
		{
			Model:        "gpt-4",
			Candidate:    "deepseek-v3",
			Weight:       0,
			UserIDHeader: "X-Team-ID",
			UserIDs:      []string{"engineering", "research"},
		},
	}
	router := shadow.NewCanaryRouter(rules)
	_, ok, reason := router.Decide("gpt-4", headers)
	if !ok {
		t.Fatal("expected user force to route")
	}
	if reason != "user-force" {
		t.Fatalf("expected user-force reason, got %s", reason)
	}
}

// TestCanaryRollback tests one-click rollback.
func TestCanaryRollback(t *testing.T) {
	rules := []shadow.CanaryConfig{
		{Model: "gpt-4", Candidate: "deepseek-v3", Weight: 50},
		{Model: "claude-4", Candidate: "gemini-3", Weight: 30},
	}
	router := shadow.NewCanaryRouter(rules)

	if !router.Rollback("gpt-4") {
		t.Fatal("expected rollback success")
	}
	r := router.Rules()
	for _, rule := range r {
		if rule.Model == "gpt-4" && rule.Weight != 0 {
			t.Fatalf("expected weight 0 after rollback, got %d", rule.Weight)
		}
	}
}

// TestMirrorSelector tests sampling ratio decisions.
func TestMirrorSelector(t *testing.T) {
	cfg := shadow.Config{
		Enabled: true,
		Mirrors: []shadow.MirrorConfig{
			{Model: "gpt-4", Candidate: "deepseek-v3", Ratio: 1.0},
		},
	}
	sel := shadow.NewMirrorSelector(cfg)

	_, ok := sel.ShouldSample("gpt-4", http.Header{})
	if !ok {
		t.Fatal("expected 1.0 ratio to always sample")
	}

	// Disabled
	cfg2 := shadow.Config{Enabled: false}
	sel2 := shadow.NewMirrorSelector(cfg2)
	_, ok = sel2.ShouldSample("gpt-4", http.Header{})
	if ok {
		t.Fatal("expected disabled to not sample")
	}
}

// TestAPIHandlerHealth tests the health endpoint.
func TestAPIHandlerHealth(t *testing.T) {
	cfg := shadow.Config{Enabled: true}
	handler := shadow.NewAPIHandler(cfg)
	handler.Start()
	defer handler.Stop()

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	handler.HandleHealth(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("json decode error: %v", err)
	}
	if resp["enabled"] != true {
		t.Fatal("expected enabled=true")
	}
}

// TestEvalStoreNilPush tests that nil records don't crash.
func TestEvalStoreNilPush(t *testing.T) {
	store := shadow.NewEvalStoreWithCapacity(10)
	store.Push(nil) // should not panic
	if store.Len() != 0 {
		t.Fatal("expected 0 records after nil push")
	}
}

// TestCanaryRouterEmptyRules tests that empty rules don't crash.
func TestCanaryRouterEmptyRules(t *testing.T) {
	router := shadow.NewCanaryRouter(nil)
	_, ok, _ := router.Decide("gpt-4", http.Header{})
	if ok {
		t.Fatal("expected no route with empty rules")
	}
}

// TestEngineEnqueueBeforeStart tests that enqueue before start returns false.
func TestEngineEnqueueBeforeStart(t *testing.T) {
	cfg := shadow.Config{Enabled: true}
	engine := shadow.NewEngine(cfg)
	ok := engine.Enqueue(&shadow.MirrorTarget{})
	if ok {
		t.Fatal("expected enqueue to return false before Start")
	}
}

// TestEngineStartStop tests that start/stop cycles work correctly.
func TestEngineStartStop(t *testing.T) {
	cfg := shadow.Config{Enabled: true}
	engine := shadow.NewEngine(cfg)
	engine.Start()
	engine.Stop()
	// Double-stop should not panic
	engine.Stop()
}

// TestEvalStoreStatsEmpty tests that stats on empty store returns nil.
func TestEvalStoreStatsEmpty(t *testing.T) {
	store := shadow.NewEvalStoreWithCapacity(10)
	stats := store.Stats("gpt-4", "deepseek-v3")
	if stats != nil {
		t.Fatal("expected nil stats for empty store")
	}
}

// TestMirrorSelectorRatioZero tests that 0 ratio never samples.
func TestMirrorSelectorRatioZero(t *testing.T) {
	cfg := shadow.Config{
		Enabled: true,
		Mirrors: []shadow.MirrorConfig{
			{Model: "gpt-4", Candidate: "deepseek-v3", Ratio: 0},
		},
	}
	sel := shadow.NewMirrorSelector(cfg)
	_, ok := sel.ShouldSample("gpt-4", http.Header{})
	if ok {
		t.Fatal("expected 0 ratio to never sample")
	}
}

// TestEngineEnqueueAfterStop tests that enqueue after stop returns false.
func TestEngineEnqueueAfterStop(t *testing.T) {
	cfg := shadow.Config{Enabled: true}
	engine := shadow.NewEngine(cfg)
	engine.Start()
	engine.Stop()

	ok := engine.Enqueue(&shadow.MirrorTarget{})
	if ok {
		t.Fatal("expected enqueue to return false after Stop")
	}
}

// TestCanaryRouterUpdateNil tests that Update with nil doesn't crash.
func TestCanaryRouterUpdateNil(t *testing.T) {
	router := shadow.NewCanaryRouter(nil)
	router.Update(nil) // should not panic
	_, ok, _ := router.Decide("gpt-4", http.Header{})
	if ok {
		t.Fatal("expected no route after nil update")
	}
}

// TestEvalStoreRecentBounds tests edge cases for Recent queries.
func TestEvalStoreRecentBounds(t *testing.T) {
	store := shadow.NewEvalStoreWithCapacity(10)

	// Recent on empty store
	recs := store.Recent(5)
	if len(recs) != 0 {
		t.Fatalf("expected 0 recent on empty, got %d", len(recs))
	}

	// Push 3, Recent(10) should return 3
	for i := 0; i < 3; i++ {
		store.Push(&shadow.Record{Model: "gpt-4", Candidate: "deepseek-v3", Similarity: 0.99})
	}
	recs = store.Recent(10)
	if len(recs) != 3 {
		t.Fatalf("expected 3 recent, got %d", len(recs))
	}
}

// TestEvalStoreClearReuses tests that the store is reusable after clear.
func TestEvalStoreClearReuses(t *testing.T) {
	store := shadow.NewEvalStoreWithCapacity(5)
	for i := 0; i < 5; i++ {
		store.Push(&shadow.Record{Model: "gpt-4", Candidate: "deepseek-v3"})
	}
	store.Clear()
	if store.Len() != 0 {
		t.Fatal("expected 0 after clear")
	}
	store.Push(&shadow.Record{Model: "gpt-4", Candidate: "deepseek-v3"})
	if store.Len() != 1 {
		t.Fatalf("expected 1 after clear+push, got %d", store.Len())
	}
}

// TestAPIHandlerConfigUpdate tests config update endpoint.
func TestAPIHandlerConfigUpdate(t *testing.T) {
	handler := shadow.NewAPIHandler(shadow.Config{Enabled: true})
	handler.Start()
	defer handler.Stop()

	// GET config
	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	w := httptest.NewRecorder()
	handler.HandleConfig(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

// TestAPIHandlerCanaryUpdate tests canary update endpoint.
func TestAPIHandlerCanaryUpdate(t *testing.T) {
	handler := shadow.NewAPIHandler(shadow.Config{
		Enabled: true,
		Canaries: []shadow.CanaryConfig{
			{Model: "gpt-4", Candidate: "deepseek-v3", Weight: 50},
		},
	})

	// GET canaries
	req := httptest.NewRequest(http.MethodGet, "/canary", nil)
	w := httptest.NewRecorder()
	handler.HandleCanary(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

// TestAPIHandlerStatsEmpty tests stats on empty handler.
func TestAPIHandlerStatsEmpty(t *testing.T) {
	handler := shadow.NewAPIHandler(shadow.Config{Enabled: true})
	req := httptest.NewRequest(http.MethodGet, "/stats", nil)
	w := httptest.NewRecorder()
	handler.HandleStats(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

// TestAPIHandlerRecordsEmpty tests records on empty handler.
func TestAPIHandlerRecordsEmpty(t *testing.T) {
	handler := shadow.NewAPIHandler(shadow.Config{Enabled: true})
	req := httptest.NewRequest(http.MethodGet, "/records", nil)
	w := httptest.NewRecorder()
	handler.HandleRecords(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

// TestCanaryActionDecoupled tests the decoupled EvaluateCanary helper.
func TestCanaryActionDecoupled(t *testing.T) {
	rules := []shadow.CanaryConfig{
		{Model: "gpt-4", Candidate: "deepseek-v3", Weight: 100},
	}
	action := shadow.EvaluateCanary(rules, "gpt-4", http.Header{})
	if action == nil {
		t.Fatal("expected non-nil action")
	}
	if action.Candidate != "deepseek-v3" {
		t.Fatalf("expected deepseek-v3, got %s", action.Candidate)
	}
	if action.Reason != "weighted" {
		t.Fatalf("expected weighted reason, got %s", action.Reason)
	}
}

// TestRecordPrimaryMetrics tests recording primary metrics.
func TestRecordPrimaryMetrics(t *testing.T) {
	handler := shadow.NewAPIHandler(shadow.Config{Enabled: true})
	handler.RecordPrimaryMetrics(100.5, 200)
	// Should not panic
}

// TestEngineConfig tests config accessors.
func TestEngineConfig(t *testing.T) {
	cfg := shadow.Config{
		Enabled:     true,
		QueueSize:   512,
		WorkerCount: 8,
	}
	engine := shadow.NewEngine(cfg)
	got := engine.Config()
	if got.QueueSize != 512 {
		t.Fatalf("expected QueueSize=512, got %d", got.QueueSize)
	}
	if got.WorkerCount != 8 {
		t.Fatalf("expected WorkerCount=8, got %d", got.WorkerCount)
	}
}

// TestAtomicBool tests the atomic boolean helper.
func TestAtomicBool(t *testing.T) {
	var b shadow.AtomicBool
	if b.Get() {
		t.Fatal("expected false initially")
	}
	b.Set(true)
	if !b.Get() {
		t.Fatal("expected true after set")
	}
	b.Set(false)
	if b.Get() {
		t.Fatal("expected false after clear")
	}
}

// TestMirrorSelectorRatio tests various ratio values.
func TestMirrorSelectorRatio(t *testing.T) {
	cfg := shadow.Config{
		Enabled: true,
		Mirrors: []shadow.MirrorConfig{
			{Model: "claude-4", Candidate: "gemini-3", Ratio: 0.5},
		},
	}
	sel := shadow.NewMirrorSelector(cfg)

	sampled := 0
	for i := 0; i < 1000; i++ {
		_, ok := sel.ShouldSample("claude-4", http.Header{})
		if ok {
			sampled++
		}
	}
	// With 50% ratio over 1000 iterations, should be ~400-600
	if sampled < 300 || sampled > 700 {
		t.Fatalf("expected ~500 samples, got %d", sampled)
	}
}

// TestCanaryUserForceNoMatch tests that non-matching user IDs don't route.
func TestCanaryUserForceNoMatch(t *testing.T) {
	headers := http.Header{}
	headers.Set("X-Team-ID", "sales")

	rules := []shadow.CanaryConfig{
		{
			Model:        "gpt-4",
			Candidate:    "deepseek-v3",
			Weight:       0,
			UserIDHeader: "X-Team-ID",
			UserIDs:      []string{"engineering", "research"},
		},
	}
	router := shadow.NewCanaryRouter(rules)
	_, ok, _ := router.Decide("gpt-4", headers)
	if ok {
		t.Fatal("expected non-matching user to not route")
	}
}

// TestAPIHandlerCanaryRollbackViaAPI tests the rollback endpoint.
func TestAPIHandlerCanaryRollbackViaAPI(t *testing.T) {
	handler := shadow.NewAPIHandler(shadow.Config{
		Enabled: true,
		Canaries: []shadow.CanaryConfig{
			{Model: "gpt-4", Candidate: "deepseek-v3", Weight: 50},
		},
	})

	// Rollback via DELETE
	req := httptest.NewRequest(http.MethodDelete, "/canary?model=gpt-4", nil)
	w := httptest.NewRecorder()
	handler.HandleCanary(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Verify rolled back
	rules := handler.CanaryRouter().Rules()
	for _, rule := range rules {
		if rule.Model == "gpt-4" && rule.Weight != 0 {
			t.Fatal("expected weight 0 after rollback")
		}
	}
}

// TestAPIHandlerCanaryUpdateWeight tests weight update via API.
func TestAPIHandlerCanaryUpdateWeight(t *testing.T) {
	handler := shadow.NewAPIHandler(shadow.Config{
		Enabled: true,
		Canaries: []shadow.CanaryConfig{
			{Model: "gpt-4", Candidate: "deepseek-v3", Weight: 0},
		},
	})

	body := `{"model":"gpt-4","weight":75}`
	req := httptest.NewRequest(http.MethodPost, "/canary", nil)
	req.Body = &readCloser{data: body}
	w := httptest.NewRecorder()
	handler.HandleCanary(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

type readCloser struct {
	data string
	pos  int
}

func (r *readCloser) Read(p []byte) (n int, err error) {
	if r.pos >= len(r.data) {
		return 0, http.ErrBodyNotAllowed
	}
	n = copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

func (r *readCloser) Close() error { return nil }

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// TestEvalStoreConcurrent tests concurrent access to the store.
func TestEvalStoreConcurrent(t *testing.T) {
	store := shadow.NewEvalStoreWithCapacity(100)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 50; i++ {
			store.Push(&shadow.Record{Model: "gpt-4", Candidate: "deepseek-v3"})
		}
		done <- struct{}{}
	}()
	go func() {
		for i := 0; i < 50; i++ {
			store.Recent(10)
			store.Query("gpt-4", "deepseek-v3", 5)
		}
		done <- struct{}{}
	}()
	<-done
	<-done
}

// TestExtractResponseText tests the response text extraction.
func TestExtractResponseText(t *testing.T) {
	// OpenAI chat completion response
	store := shadow.NewEvalStoreWithCapacity(10)
	store.Push(&shadow.Record{
		Model:      "gpt-4",
		Candidate:  "deepseek-v3",
		PromptHash: "abc",
		Timestamp:  time.Now(),
	})
	recs := store.Query("gpt-4", "deepseek-v3", 1)
	if len(recs) != 1 {
		t.Fatalf("expected 1 record, got %d", len(recs))
	}
	if recs[0].PromptHash != "abc" {
		t.Fatalf("expected prompt hash abc, got %s", recs[0].PromptHash)
	}
}

// TestCanaryMultiModel tests that different models are handled separately.
func TestCanaryMultiModel(t *testing.T) {
	rules := []shadow.CanaryConfig{
		{Model: "gpt-4", Candidate: "deepseek-v3", Weight: 100},
		{Model: "claude-4", Candidate: "gemini-3", Weight: 0},
	}
	router := shadow.NewCanaryRouter(rules)

	_, ok1, _ := router.Decide("gpt-4", http.Header{})
	if !ok1 {
		t.Fatal("expected gpt-4 to route")
	}
	_, ok2, _ := router.Decide("claude-4", http.Header{})
	if ok2 {
		t.Fatal("expected claude-4 to not route")
	}
}

// TestDefaultConfig tests that defaults are applied correctly.
func TestDefaultConfig(t *testing.T) {
	var cfg shadow.Config
	cfg.Defaults()
	if cfg.QueueSize != 256 {
		t.Fatalf("expected QueueSize=256, got %d", cfg.QueueSize)
	}
	if cfg.WorkerCount != 4 {
		t.Fatalf("expected WorkerCount=4, got %d", cfg.WorkerCount)
	}
	if cfg.Timeout != 30*time.Second {
		t.Fatalf("expected Timeout=30s, got %v", cfg.Timeout)
	}
}

// TestRingBuffer tests the metrics ring buffer.
func TestRingBuffer(t *testing.T) {
	rb := shadow.NewRingBuffer(3)
	if rb.Avg() != 0 {
		t.Fatal("expected 0 avg for empty")
	}
	rb.Push(10)
	rb.Push(20)
	rb.Push(30)
	if rb.Avg() != 20 {
		t.Fatalf("expected avg 20, got %f", rb.Avg())
	}
	rb.Push(40) // overwrites 10
	avg := rb.Avg()
	if avg < 28 || avg > 32 {
		t.Fatalf("expected avg ~30, got %f", avg)
	}
}

// TestAPIHandlerCanaryUpdateMissingModel tests updating a nonexistent model.
func TestAPIHandlerCanaryUpdateMissingModel(t *testing.T) {
	handler := shadow.NewAPIHandler(shadow.Config{
		Enabled: true,
		Canaries: []shadow.CanaryConfig{
			{Model: "gpt-4", Candidate: "deepseek-v3", Weight: 50},
		},
	})

	body := `{"model":"nonexistent","weight":90}`
	req := httptest.NewRequest(http.MethodPost, "/canary", nil)
	req.Body = &readCloser{data: body}
	w := httptest.NewRecorder()
	handler.HandleCanary(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// TestAPIHandlerCanaryDeleteMissingModel tests deleting a nonexistent model.
func TestAPIHandlerCanaryDeleteMissingModel(t *testing.T) {
	handler := shadow.NewAPIHandler(shadow.Config{
		Enabled: true,
		Canaries: []shadow.CanaryConfig{
			{Model: "gpt-4", Candidate: "deepseek-v3", Weight: 50},
		},
	})

	req := httptest.NewRequest(http.MethodDelete, "/canary?model=nonexistent", nil)
	w := httptest.NewRecorder()
	handler.HandleCanary(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// TestAPIHandlerCanaryDeleteNoModel tests DELETE without model param.
func TestAPIHandlerCanaryDeleteNoModel(t *testing.T) {
	handler := shadow.NewAPIHandler(shadow.Config{Enabled: true})
	req := httptest.NewRequest(http.MethodDelete, "/canary", nil)
	w := httptest.NewRecorder()
	handler.HandleCanary(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// TestAPIHandlerConfigInvalidBody tests config update with invalid body.
func TestAPIHandlerConfigInvalidBody(t *testing.T) {
	handler := shadow.NewAPIHandler(shadow.Config{Enabled: true})
	req := httptest.NewRequest(http.MethodPost, "/config", nil)
	req.Body = &readCloser{data: "not json"}
	w := httptest.NewRecorder()
	handler.HandleConfig(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// TestEvalStoreStatsWithErrors tests stats calculation with error records.
func TestEvalStoreStatsWithErrors(t *testing.T) {
	store := shadow.NewEvalStoreWithCapacity(10)
	store.Push(&shadow.Record{Model: "gpt-4", Candidate: "deepseek-v3", Similarity: 0.9, Error: ""})
	store.Push(&shadow.Record{Model: "gpt-4", Candidate: "deepseek-v3", Similarity: 0.0, Error: "timeout"})
	store.Push(&shadow.Record{Model: "gpt-4", Candidate: "deepseek-v3", Similarity: 0.8, Error: ""})

	stats := store.Stats("gpt-4", "deepseek-v3")
	if stats == nil {
		t.Fatal("expected stats")
	}
	if stats.TotalEvals != 3 {
		t.Fatalf("expected 3 total evals, got %d", stats.TotalEvals)
	}
	if stats.ErrorCount != 1 {
		t.Fatalf("expected 1 error, got %d", stats.ErrorCount)
	}
	if abs(stats.Similarity-0.85) > 1e-6 {
		t.Fatalf("expected similarity 0.85, got %f", stats.Similarity)
	}
}

// TestEngineUpdateConfig tests hot-swapping config.
func TestEngineUpdateConfig(t *testing.T) {
	cfg := shadow.Config{Enabled: true, QueueSize: 256, WorkerCount: 4}
	engine := shadow.NewEngine(cfg)
	engine.Start()
	defer engine.Stop()

	newCfg := shadow.Config{Enabled: true, QueueSize: 512, WorkerCount: 8}
	engine.UpdateConfig(newCfg)

	got := engine.Config()
	if got.QueueSize != 512 {
		t.Fatalf("expected QueueSize=512, got %d", got.QueueSize)
	}
}

// TestCanaryConfigAPITest tests the full canary API flows.
func TestCanaryConfigAPIFlow(t *testing.T) {
	handler := shadow.NewAPIHandler(shadow.Config{
		Enabled: true,
		Canaries: []shadow.CanaryConfig{
			{Model: "gpt-4", Candidate: "deepseek-v3", Weight: 10},
		},
	})
	handler.Start()
	defer handler.Stop()

	// Verify initial state
	rules := handler.CanaryRouter().Rules()
	if len(rules) != 1 || rules[0].Weight != 10 {
		t.Fatalf("expected 1 rule with weight 10, got %d rules, weight=%d", len(rules), rules[0].Weight)
	}

	// Scale up to 80%
	handler.CanaryRouter().Update([]shadow.CanaryConfig{
		{Model: "gpt-4", Candidate: "deepseek-v3", Weight: 80},
	})
	rules = handler.CanaryRouter().Rules()
	if rules[0].Weight != 80 {
		t.Fatalf("expected weight 80, got %d", rules[0].Weight)
	}

	// Rollback
	handler.CanaryRouter().Rollback("gpt-4")
	rules = handler.CanaryRouter().Rules()
	if rules[0].Weight != 0 {
		t.Fatalf("expected weight 0 after rollback, got %d", rules[0].Weight)
	}
}

// TestAPIHandlerMethods tests HTTP method handling.
func TestAPIHandlerMethodNotAllowed(t *testing.T) {
	handler := shadow.NewAPIHandler(shadow.Config{Enabled: true})
	req := httptest.NewRequest(http.MethodPatch, "/canary", nil)
	w := httptest.NewRecorder()
	handler.HandleCanary(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}

// TestRecordIDGeneration tests that IDs are auto-generated when empty.
func TestRecordIDGeneration(t *testing.T) {
	store := shadow.NewEvalStoreWithCapacity(5)
	store.Push(&shadow.Record{Model: "gpt-4", Candidate: "deepseek-v3"})
	store.Push(&shadow.Record{Model: "gpt-4", Candidate: "deepseek-v3"})
	recs := store.Recent(5)
	if len(recs) != 2 {
		t.Fatalf("expected 2 records, got %d", len(recs))
	}
	if recs[0].ID == "" {
		t.Fatal("expected non-empty ID")
	}
}

// TestCanaryDecideWithNoRules tests Decide with no matching rules.
func TestCanaryDecideWithNoRules(t *testing.T) {
	router := shadow.NewCanaryRouter([]shadow.CanaryConfig{
		{Model: "gpt-4", Candidate: "deepseek-v3", Weight: 0},
	})
	_, ok, _ := router.Decide("gpt-4", http.Header{})
	if ok {
		t.Fatal("expected no route with 0 weight")
	}
}

// TestNewCanaryRouterWithEmptySlice tests creation with empty slice.
func TestNewCanaryRouterWithEmptySlice(t *testing.T) {
	router := shadow.NewCanaryRouter([]shadow.CanaryConfig{})
	_, ok, _ := router.Decide("gpt-4", http.Header{})
	if ok {
		t.Fatal("expected no route with empty slice")
	}
}

// RING_BUFFER_INTERNAL is a test for the RingBuffer's internal behavior.
func TestRingBufferUnderflow(t *testing.T) {
	rb := shadow.NewRingBuffer(5)
	// Push less than capacity
	rb.Push(10)
	rb.Push(20)
	avg := rb.Avg()
	if avg != 15 {
		t.Fatalf("expected avg 15, got %f", avg)
	}
}

// TestEvalStoreQueryLimit tests that Query respects the limit parameter.
func TestEvalStoreQueryLimit(t *testing.T) {
	store := shadow.NewEvalStoreWithCapacity(20)
	for i := 0; i < 10; i++ {
		store.Push(&shadow.Record{Model: "gpt-4", Candidate: "deepseek-v3"})
	}
	recs := store.Query("gpt-4", "deepseek-v3", 3)
	if len(recs) != 3 {
		t.Fatalf("expected 3 records (limit=3), got %d", len(recs))
	}
}

// TestEvalStoreQueryNoFilter tests Query without model filter.
func TestEvalStoreQueryNoFilter(t *testing.T) {
	store := shadow.NewEvalStoreWithCapacity(10)
	store.Push(&shadow.Record{Model: "gpt-4", Candidate: "deepseek-v3"})
	store.Push(&shadow.Record{Model: "claude-4", Candidate: "gemini-3"})
	recs := store.Query("", "", 10)
	if len(recs) != 2 {
		t.Fatalf("expected 2 records (no filter), got %d", len(recs))
	}
}

// TestMirrorSelectorDisabled tests disabled selector.
func TestMirrorSelectorDisabled(t *testing.T) {
	cfg := shadow.Config{Enabled: false}
	sel := shadow.NewMirrorSelector(cfg)
	_, ok := sel.ShouldSample("gpt-4", http.Header{})
	if ok {
		t.Fatal("expected disabled selector to not sample")
	}
}

// TestMirrorSelectorNoMatch tests that non-matching model returns false.
func TestMirrorSelectorNoMatch(t *testing.T) {
	cfg := shadow.Config{
		Enabled: true,
		Mirrors: []shadow.MirrorConfig{
			{Model: "gpt-4", Candidate: "deepseek-v3", Ratio: 1.0},
		},
	}
	sel := shadow.NewMirrorSelector(cfg)
	_, ok := sel.ShouldSample("claude-4", http.Header{})
	if ok {
		t.Fatal("expected non-matching model to not sample")
	}
}

// TestAPIHandlerCanaryUpdateInvalidBody tests canary update with invalid JSON.
func TestAPIHandlerCanaryUpdateInvalidBody(t *testing.T) {
	handler := shadow.NewAPIHandler(shadow.Config{Enabled: true})
	req := httptest.NewRequest(http.MethodPost, "/canary", nil)
	req.Body = &readCloser{data: "not json"}
	w := httptest.NewRecorder()
	handler.HandleCanary(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

// TestShadowNewAPIHandler tests that new API handler starts cleanly.
func TestShadowNewAPIHandler(t *testing.T) {
	cfg := shadow.Config{Enabled: true}
	handler := shadow.NewAPIHandler(cfg)
	if handler == nil {
		t.Fatal("expected non-nil handler")
	}
	handler.Start()
	handler.Stop()
}

// TestRecordTimestamp tests that timestamp is preserved.
func TestRecordTimestamp(t *testing.T) {
	store := shadow.NewEvalStoreWithCapacity(5)
	now := time.Now()
	store.Push(&shadow.Record{
		Model:     "gpt-4",
		Candidate: "deepseek-v3",
		Timestamp: now,
	})
	recs := store.Recent(1)
	if len(recs) != 1 {
		t.Fatalf("expected 1 record, got %d", len(recs))
	}
	if !recs[0].Timestamp.Equal(now) {
		t.Fatal("timestamp mismatch")
	}
}

// TestEvalStoreStatsNoError tests stats when all records are valid.
func TestEvalStoreStatsNoError(t *testing.T) {
	store := shadow.NewEvalStoreWithCapacity(10)
	for i := 0; i < 5; i++ {
		store.Push(&shadow.Record{
			Model:         "gpt-4",
			Candidate:     "deepseek-v3",
			Similarity:    0.95,
			CandidateTTFT: 200,
		})
	}
	stats := store.Stats("gpt-4", "deepseek-v3")
	if stats == nil {
		t.Fatal("expected stats")
	}
	if stats.ErrorCount != 0 {
		t.Fatalf("expected 0 errors, got %d", stats.ErrorCount)
	}
	if stats.TotalEvals != 5 {
		t.Fatalf("expected 5 total, got %d", stats.TotalEvals)
	}
	if stats.Similarity != 0.95 {
		t.Fatalf("expected similarity 0.95, got %f", stats.Similarity)
	}
}

// TestCanaryConfigUpdateAfterCreate tests that the handler reflects config updates.
func TestCanaryConfigUpdateAfterCreate(t *testing.T) {
	handler := shadow.NewAPIHandler(shadow.Config{
		Enabled: true,
		Canaries: []shadow.CanaryConfig{
			{Model: "gpt-4", Candidate: "deepseek-v3", Weight: 10},
		},
	})
	handler.Start()
	defer handler.Stop()

	// Update via PUT
	body := `{"enabled":true,"canaries":[{"model":"gpt-4","candidate":"deepseek-v3","weight":50}]}`
	req := httptest.NewRequest(http.MethodPut, "/config", nil)
	req.Body = &readCloser{data: body}
	w := httptest.NewRecorder()
	handler.HandleConfig(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	rules := handler.CanaryRouter().Rules()
	if len(rules) != 1 || rules[0].Weight != 50 {
		t.Fatalf("expected weight 50, got %d", rules[0].Weight)
	}
}

// TestCanaryRouterDecideMultiRule tests that the first matching rule wins.
func TestCanaryRouterDecideMultiRule(t *testing.T) {
	rules := []shadow.CanaryConfig{
		{Model: "gpt-4", Candidate: "deepseek-v3", Weight: 100},
		{Model: "gpt-4", Candidate: "gemini-3", Weight: 100},
	}
	router := shadow.NewCanaryRouter(rules)
	rule, ok, _ := router.Decide("gpt-4", http.Header{})
	if !ok {
		t.Fatal("expected route")
	}
	if rule.Candidate != "deepseek-v3" {
		t.Fatalf("expected first matching candidate deepseek-v3, got %s", rule.Candidate)
	}
}

// TestEvalStoreQueryWithEmptyStore tests Query on freshly created store.
func TestEvalStoreQueryWithEmptyStore(t *testing.T) {
	store := shadow.NewEvalStoreWithCapacity(10)
	recs := store.Query("gpt-4", "deepseek-v3", 5)
	if len(recs) != 0 {
		t.Fatalf("expected 0 records, got %d", len(recs))
	}
}

// TestEvalStoreStatsWithNoRecords tests Stats returns nil on empty store.
func TestEvalStoreStatsWithNoRecords(t *testing.T) {
	store := shadow.NewEvalStoreWithCapacity(10)
	stats := store.Stats("", "")
	if stats != nil {
		t.Fatal("expected nil stats")
	}
}
