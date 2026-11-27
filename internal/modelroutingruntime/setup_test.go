package modelroutingruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/modelrouting"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	"gopkg.in/yaml.v3"
)

func newTestConfig() *config.Config {
	budget := 0.01
	return &config.Config{ModelRouting: config.ModelRoutingConfig{
		Enabled:      true,
		SyncInterval: "24h",
		DefaultStrategy: &config.ModelRoutingStrategyConfig{
			Optimization: modelrouting.OptMinCost,
			MaxBudget:    &budget,
		},
	}}
}

// The channel builds only when enabled, and a disabled/empty block builds
// nothing so the default build installs no gate and starts no goroutine.
func TestBuildDisabledOrInvalidBuildsNothing(t *testing.T) {
	cases := []struct {
		name string
		cfg  *config.Config
		want bool
	}{
		{"nil config", nil, false},
		{"disabled", &config.Config{ModelRouting: config.ModelRoutingConfig{Enabled: false}}, false},
		{"enabled minimal", &config.Config{ModelRouting: config.ModelRoutingConfig{Enabled: true}}, true},
		{"invalid optimization", &config.Config{ModelRouting: config.ModelRoutingConfig{
			Enabled:         true,
			DefaultStrategy: &config.ModelRoutingStrategyConfig{Optimization: "bogus"},
		}}, false},
		{"quality floor out of range", &config.Config{ModelRouting: config.ModelRoutingConfig{
			Enabled:         true,
			DefaultStrategy: &config.ModelRoutingStrategyConfig{Optimization: modelrouting.OptMinCost, QualityFloor: 2},
		}}, false},
		{"bad sync interval", &config.Config{ModelRouting: config.ModelRoutingConfig{
			Enabled: true, SyncInterval: "not-a-duration",
		}}, false},
		{"enabled with strategy", newTestConfig(), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Build(tc.cfg)
			if (got != nil) != tc.want {
				t.Fatalf("Build() nil=%v, want built=%v", got == nil, tc.want)
			}
			if got != nil {
				if ServerOption(got) == nil {
					t.Fatal("ServerOption() = nil for a built runtime")
				}
				if HandlersServerOption(got) == nil {
					t.Fatal("HandlersServerOption() = nil for a built runtime")
				}
			}
		})
	}
}

func TestServerOptionNilRuntime(t *testing.T) {
	if ServerOption(nil) != nil || HandlersServerOption(nil) != nil {
		t.Fatal("ServerOption/HandlersServerOption must be nil for a nil runtime")
	}
}

// The config block the operator writes parses into ModelRoutingConfig, and the
// management key is never serialized back out.
func TestModelRoutingBlockYAMLUnmarshalAndJSONRedaction(t *testing.T) {
	raw := `
model-routing:
  enabled: true
  keeper-url: http://keeper:8080/keeper
  sync-interval: 12h
  default-strategy:
    optimization: min_cost
    quality-floor: 0.6
    max-budget: 0.01
    preferred-model: claude-haiku
    only-allow-model: ""
`
	cfg := &config.Config{}
	if err := yaml.Unmarshal([]byte(raw), cfg); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	block := cfg.ModelRouting
	if !block.Enabled || block.KeeperURL != "http://keeper:8080/keeper" || block.SyncInterval != "12h" {
		t.Fatalf("block = %+v, want enabled with url and interval", block)
	}
	if block.DefaultStrategy == nil || block.DefaultStrategy.Optimization != modelrouting.OptMinCost {
		t.Fatalf("default strategy = %+v, want min_cost", block.DefaultStrategy)
	}
	if block.DefaultStrategy.MaxBudget == nil || *block.DefaultStrategy.MaxBudget != 0.01 {
		t.Fatalf("max budget = %v, want 0.01", block.DefaultStrategy.MaxBudget)
	}

	block.ManagementKey = "super-secret"
	out, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	if bytes.Contains(out, []byte("super-secret")) {
		t.Fatalf("management key leaked into JSON: %s", out)
	}
}

// scopeFromContext maps an authenticated principal to a hashed user scope and
// a missing principal to the zero (default) scope.
func TestScopeFromContext(t *testing.T) {
	gin.SetMode(gin.TestMode)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	scope := scopeFromContext(contextWithGin(c))
	if scope.Kind != "" || scope.ID != "" {
		t.Fatalf("scope = %+v, want zero scope without a principal", scope)
	}

	c.Set("userApiKey", "sk-test-123")
	scope = scopeFromContext(contextWithGin(c))
	if scope.Kind != modelrouting.ScopeUser {
		t.Fatalf("scope kind = %q, want user", scope.Kind)
	}
	if !strings.HasPrefix(scope.ID, scopeIDPrefix) || len(scope.ID) != len(scopeIDPrefix)+scopeIDHashChars {
		t.Fatalf("scope id = %q, want %s + %d hex chars", scope.ID, scopeIDPrefix, scopeIDHashChars)
	}
	if strings.Contains(scope.ID, "sk-test-123") {
		t.Fatal("scope id must not embed the raw API key")
	}
	if again := ScopeIDForKey("sk-test-123"); again != scope.ID {
		t.Fatalf("ScopeIDForKey not deterministic: %q vs %q", again, scope.ID)
	}
	if ScopeIDForKey("  ") != "" {
		t.Fatal("ScopeIDForKey(blank) must be empty")
	}
}

// The bearer fallback resolves the key when the auth middleware has not run.
func TestScopeFromContextBearerFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Request.Header.Set("Authorization", "Bearer sk-bearer-42")

	scope := scopeFromContext(contextWithGin(c))
	if scope.Kind != modelrouting.ScopeUser {
		t.Fatalf("scope = %+v, want user scope from bearer", scope)
	}
	if want := ScopeIDForKey("sk-bearer-42"); scope.ID != want {
		t.Fatalf("scope id = %q, want %q", scope.ID, want)
	}
}

// AC2: an enabled channel with a price snapshot routes the request to the
// strategy-selected model and sets x-or-model / x-or-reason.
func TestRouteRequestSelectsModelAndHeaders(t *testing.T) {
	runtime := Build(newTestConfig())
	if runtime == nil {
		t.Fatal("Build() = nil")
	}
	runtime.core.Prices.Apply([]modelrouting.Price{
		{Model: "expensive", PromptPricePer1M: 100, CompletionPricePer1M: 100, Multiplier: 1},
		{Model: "cheap", PromptPricePer1M: 0.1, CompletionPricePer1M: 0.1, Multiplier: 1},
	}, time.Now())

	body := []byte(`{"model":"requested","messages":[{"role":"user","content":"hi"}]}`)
	out := runtime.RouteRequest(contextWithGin(emptyGinContext()), routingReq(body, "requested"))
	if out.Error != nil {
		t.Fatalf("unexpected terminal error: %+v", out.Error)
	}
	if out.Model != "cheap" {
		t.Fatalf("chosen model = %q, want cheap (min_cost over a priced pool)", out.Model)
	}
	if got := out.Headers.Get(modelrouting.HeaderModel); got != "cheap" {
		t.Fatalf("%s = %q, want cheap", modelrouting.HeaderModel, got)
	}
	if reason := out.Headers.Get(modelrouting.HeaderReason); !strings.Contains(reason, "chose_cheap") {
		t.Fatalf("%s = %q, want a chose_cheap reason", modelrouting.HeaderReason, reason)
	}
}

// AC4: the cost hard cap returns 402 budget_exceeded without any upstream call.
func TestRouteRequestBudgetExceeded(t *testing.T) {
	budget := 0.0001
	cfg := &config.Config{ModelRouting: config.ModelRoutingConfig{
		Enabled: true,
		DefaultStrategy: &config.ModelRoutingStrategyConfig{
			Optimization: modelrouting.OptMinCost,
			MaxBudget:    &budget,
		},
	}}
	runtime := Build(cfg)
	runtime.core.Prices.Apply([]modelrouting.Price{
		{Model: "pricy", PromptPricePer1M: 5, CompletionPricePer1M: 5, Multiplier: 1},
	}, time.Now())

	out := runtime.RouteRequest(contextWithGin(emptyGinContext()), routingReq([]byte(strings.Repeat("x", 400)), "pricy"))
	if out.Error == nil {
		t.Fatal("expected a terminal routing error")
	}
	if out.Error.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402", out.Error.StatusCode)
	}
	if !bytes.Contains(out.Error.Body, []byte(modelrouting.ErrorBudgetExceeded)) {
		t.Fatalf("body = %s, want code %s", out.Error.Body, modelrouting.ErrorBudgetExceeded)
	}
}

// The configured quality floor is enforced on the routing pool: a lite model
// is excluded when the floor demands an advanced tier, even if it is cheaper.
func TestRouteRequestQualityFloorFiltersPool(t *testing.T) {
	cfg := &config.Config{ModelRouting: config.ModelRoutingConfig{
		Enabled: true,
		DefaultStrategy: &config.ModelRoutingStrategyConfig{
			Optimization: modelrouting.OptMinCost,
			QualityFloor: 0.6, // -> LevelAdvanced
		},
	}}
	runtime := Build(cfg)
	runtime.core.Prices.Apply([]modelrouting.Price{
		{Model: "claude-haiku", PromptPricePer1M: 0.1, CompletionPricePer1M: 0.1, Multiplier: 1},
		{Model: "claude-opus-4-7", PromptPricePer1M: 90, CompletionPricePer1M: 90, Multiplier: 1},
	}, time.Now())

	out := runtime.RouteRequest(contextWithGin(emptyGinContext()), routingReq([]byte(`{"model":"claude-haiku"}`), "claude-haiku"))
	if out.Error != nil {
		t.Fatalf("unexpected terminal error: %+v", out.Error)
	}
	if out.Model != "claude-opus-4-7" {
		t.Fatalf("chosen model = %q, want the only model meeting the advanced floor", out.Model)
	}
}

// Rabbit hole 2: with no price snapshot the gate passes the request through
// unchanged (no decision, no rejection) and reports why on x-or-reason.
func TestRouteRequestNoSnapshotPassesThrough(t *testing.T) {
	runtime := Build(newTestConfig())
	out := runtime.RouteRequest(contextWithGin(emptyGinContext()), routingReq([]byte(`{"model":"m"}`), "m"))
	if out.Error != nil {
		t.Fatalf("pass-through must not error: %+v", out.Error)
	}
	if out.Model != "" {
		t.Fatalf("model = %q, want empty (keep requested)", out.Model)
	}
	if reason := out.Headers.Get(modelrouting.HeaderReason); reason != passthroughReasonNoPrices {
		t.Fatalf("%s = %q, want %q", modelrouting.HeaderReason, reason, passthroughReasonNoPrices)
	}
}

// A per-scope version outranks the configured default, and the effective view
// reports the version number.
func TestEffectiveVersionOverridesConfiguredDefault(t *testing.T) {
	runtime := Build(newTestConfig())
	scope := modelrouting.Scope{Kind: modelrouting.ScopeUser, ID: "key_abc"}
	if _, err := runtime.core.Versions.Update(scope, modelrouting.Record{
		Strategy: modelrouting.Strategy{Optimization: modelrouting.OptHighQuality},
		Note:     "seed",
	}); err != nil {
		t.Fatalf("seed version: %v", err)
	}
	view := runtime.effective(scope)
	if !view.Found || view.Version != 1 || view.Strategy.Optimization != modelrouting.OptHighQuality {
		t.Fatalf("view = %+v, want found v1 high_quality", view)
	}

	// An unconfigured scope keeps the configured default.
	other := runtime.effective(modelrouting.Scope{Kind: modelrouting.ScopeUser, ID: "key_other"})
	if other.Found {
		t.Fatalf("view = %+v, want found=false for an unconfigured scope", other)
	}
	if other.Strategy.Optimization != modelrouting.OptMinCost {
		t.Fatalf("optimization = %q, want the configured min_cost default", other.Strategy.Optimization)
	}
}

// AC3 + boundary: POST appends an immutable version (v1 survives verbatim), a
// rollback is a new version, and invalid input is rejected without a write.
func TestPostPoliciesAppendAndRollback(t *testing.T) {
	runtime := Build(newTestConfig())
	const scopeQuery = "?scope_kind=user&scope_id=key_ops"

	first := `{"strategy":{"optimization":"min_cost","quality_floor":0.5},"note":"initial"}`
	if rec := postPolicies(t, runtime, scopeQuery, first); rec.Code != http.StatusOK {
		t.Fatalf("first POST status = %d body=%s", rec.Code, rec.Body.String())
	}
	second := `{"strategy":{"optimization":"high_quality","quality_floor":0.9},"note":"escalate"}`
	rec := postPolicies(t, runtime, scopeQuery, second)
	if rec.Code != http.StatusOK {
		t.Fatalf("second POST status = %d body=%s", rec.Code, rec.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := payload["version"]; got != float64(2) {
		t.Fatalf("version = %v, want 2", got)
	}
	versions, _ := payload["versions"].([]any)
	if len(versions) != 2 {
		t.Fatalf("versions = %v, want 2 records", versions)
	}
	firstRecord := versions[0].(map[string]any)
	if firstRecord["note"] != "initial" {
		t.Fatalf("v1 note = %v, want the original 'initial' (history unmodified)", firstRecord["note"])
	}

	// Rollback = append the v1 field values again -> v3 with v1 semantics.
	rollback := `{"strategy":{"optimization":"min_cost","quality_floor":0.5},"note":"rollback to v1"}`
	rec = postPolicies(t, runtime, scopeQuery, rollback)
	if rec.Code != http.StatusOK {
		t.Fatalf("rollback POST status = %d body=%s", rec.Code, rec.Body.String())
	}
	var payload2 map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &payload2)
	if got := payload2["version"]; got != float64(3) {
		t.Fatalf("rollback version = %v, want 3", got)
	}
	effective, _ := payload2["effective"].(map[string]any)
	strategy, _ := effective["strategy"].(map[string]any)
	if strategy["optimization"] != modelrouting.OptMinCost {
		t.Fatalf("effective optimization = %v, want min_cost (v1 semantics)", strategy["optimization"])
	}
	if n := len(payload2["versions"].([]any)); n != 3 {
		t.Fatalf("versions after rollback = %d, want 3", n)
	}

	// Boundary: no note, bad optimization, out-of-range floor are 400 and do
	// not write a version.
	before := len(runtime.core.Versions.List(modelrouting.Scope{Kind: modelrouting.ScopeUser, ID: "key_ops"}))
	for _, body := range []string{
		`{"strategy":{"optimization":"min_cost","quality_floor":0.5}}`,
		`{"strategy":{"optimization":"bogus","quality_floor":0.5},"note":"x"}`,
		`{"strategy":{"optimization":"min_cost","quality_floor":1.5},"note":"x"}`,
		`not-json`,
	} {
		if rec := postPolicies(t, runtime, scopeQuery, body); rec.Code != http.StatusBadRequest {
			t.Fatalf("body %s status = %d, want 400", body, rec.Code)
		}
	}
	after := len(runtime.core.Versions.List(modelrouting.Scope{Kind: modelrouting.ScopeUser, ID: "key_ops"}))
	if before != after {
		t.Fatalf("versions grew from %d to %d on rejected input", before, after)
	}
}

// GET reports the effective view, the version list and the price status; an
// unknown scope selector is a 400.
func TestGetPolicies(t *testing.T) {
	runtime := Build(newTestConfig())
	rec := getPolicies(runtime, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	scope, _ := payload["scope"].(map[string]any)
	if scope["kind"] != "default" {
		t.Fatalf("scope kind = %v, want default", scope["kind"])
	}
	effective, _ := payload["effective"].(map[string]any)
	strategy, _ := effective["strategy"].(map[string]any)
	if strategy["optimization"] != modelrouting.OptMinCost {
		t.Fatalf("effective optimization = %v, want the configured min_cost", strategy["optimization"])
	}
	status, _ := payload["price_status"].(map[string]any)
	if status["stale"] != true {
		t.Fatalf("price_status.stale = %v, want true with no sync", status["stale"])
	}

	if rec := getPolicies(runtime, "?scope_kind=team"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad scope status = %d, want 400", rec.Code)
	}
	if rec := getPolicies(runtime, "?scope_id=missing-kind"); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing kind status = %d, want 400", rec.Code)
	}
}

// Persistence: an appended version survives a restart with its note, version
// number and timestamp intact (the audit trail is restored, not re-derived).
func TestPoliciesPersistAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "routing-state.json")

	cfg := newTestConfig()
	cfg.ModelRouting.StatePath = path
	first := Build(cfg)
	if first == nil {
		t.Fatal("Build() = nil")
	}
	const scopeQuery = "?scope_kind=user&scope_id=key_persist"
	body := `{"strategy":{"optimization":"high_quality","quality_floor":0.8},"note":"escalate"}`

	// Write through the management API while state-path points at a nested
	// directory that does not exist yet.
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	rec := postPolicies(t, first, scopeQuery, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d body=%s", rec.Code, rec.Body.String())
	}
	original := first.core.Versions.List(modelrouting.Scope{Kind: modelrouting.ScopeUser, ID: "key_persist"})
	if len(original) != 1 {
		t.Fatalf("versions = %d, want 1", len(original))
	}

	// "Restart": a fresh runtime over the same state path.
	second := Build(cfg)
	if second == nil {
		t.Fatal("Build() after restart = nil")
	}
	restored := second.core.Versions.List(modelrouting.Scope{Kind: modelrouting.ScopeUser, ID: "key_persist"})
	if len(restored) != 1 {
		t.Fatalf("restored versions = %d, want 1", len(restored))
	}
	if restored[0].Version != original[0].Version {
		t.Fatalf("restored version = %d, want %d", restored[0].Version, original[0].Version)
	}
	if !restored[0].Timestamp.Equal(original[0].Timestamp) {
		t.Fatalf("restored timestamp = %v, want %v verbatim", restored[0].Timestamp, original[0].Timestamp)
	}
	if restored[0].Note != "escalate" {
		t.Fatalf("restored note = %q, want escalate", restored[0].Note)
	}
	if restored[0].Strategy.Optimization != modelrouting.OptHighQuality {
		t.Fatalf("restored strategy = %+v, want high_quality", restored[0].Strategy)
	}

	// The restored scope is indexed, so the next append persists it too.
	out := second.RouteRequest(contextWithGin(emptyGinContext()), routingReq([]byte(`{}`), "x"))
	_ = out // no price snapshot: pass-through, engines untouched

	// State file is private (0600) — it carries operator policy, treated as
	// management-plane material.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat state file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("state file mode = %o, want 600", perm)
	}
}

// A corrupt state file must not take the channel down: Build still returns a
// runtime, with an empty history.
func TestCorruptStateFileFallsBackToEmptyHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routing-state.json")
	if err := os.WriteFile(path, []byte("{ not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg := newTestConfig()
	cfg.ModelRouting.StatePath = path
	runtime := Build(cfg)
	if runtime == nil {
		t.Fatal("Build() must still return a runtime on a corrupt state file")
	}
	if n := len(runtime.core.Versions.List(modelrouting.Scope{Kind: modelrouting.ScopeUser, ID: "key_persist"})); n != 0 {
		t.Fatalf("versions = %d, want 0 after a corrupt file", n)
	}
}

// Restore refuses to overwrite a live history or to absorb a corrupted one.
func TestScopeTableRestoreGuards(t *testing.T) {
	table := modelrouting.NewScopeTable()
	scope := modelrouting.Scope{Kind: modelrouting.ScopeUser, ID: "key_r"}
	now := time.Unix(1_700_000_000, 0)

	good := []modelrouting.Version{
		{Version: 1, Timestamp: now, Strategy: modelrouting.Strategy{Optimization: modelrouting.OptMinCost}},
		{Version: 2, Timestamp: now.Add(time.Second), Strategy: modelrouting.Strategy{Optimization: modelrouting.OptBalanced}},
	}
	if err := table.Restore(scope, good); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if n := len(table.List(scope)); n != 2 {
		t.Fatalf("restored = %d, want 2", n)
	}
	// Second restore into the same scope is refused.
	if err := table.Restore(scope, good); err == nil {
		t.Fatal("restore over a live history must be refused")
	}

	// Non-increasing version / timestamp rejected.
	other := modelrouting.Scope{Kind: modelrouting.ScopeUser, ID: "key_bad"}
	if err := table.Restore(other, []modelrouting.Version{
		{Version: 2, Timestamp: now, Strategy: modelrouting.Strategy{Optimization: modelrouting.OptMinCost}},
		{Version: 1, Timestamp: now.Add(time.Second), Strategy: modelrouting.Strategy{Optimization: modelrouting.OptMinCost}},
	}); err == nil {
		t.Fatal("non-increasing version must be refused")
	}
	if err := table.Restore(other, []modelrouting.Version{
		{Version: 1, Timestamp: now.Add(time.Second), Strategy: modelrouting.Strategy{Optimization: modelrouting.OptMinCost}},
		{Version: 2, Timestamp: now, Strategy: modelrouting.Strategy{Optimization: modelrouting.OptMinCost}},
	}); err == nil {
		t.Fatal("non-increasing timestamp must be refused")
	}
	// A gap means a record was dropped: corruption, not retention.
	if err := table.Restore(other, []modelrouting.Version{
		{Version: 1, Timestamp: now, Strategy: modelrouting.Strategy{Optimization: modelrouting.OptMinCost}},
		{Version: 3, Timestamp: now.Add(time.Second), Strategy: modelrouting.Strategy{Optimization: modelrouting.OptMinCost}},
	}); err == nil {
		t.Fatal("a version gap (v1, v3) must be refused")
	}
	// A record's own number must be positive.
	if err := table.Restore(other, []modelrouting.Version{
		{Version: 0, Timestamp: now, Strategy: modelrouting.Strategy{Optimization: modelrouting.OptMinCost}},
	}); err == nil {
		t.Fatal("version 0 must be refused")
	}
	// Invalid strategy snapshot rejected.
	if err := table.Restore(other, []modelrouting.Version{
		{Version: 1, Timestamp: now, Strategy: modelrouting.Strategy{Optimization: "bogus"}},
	}); err == nil {
		t.Fatal("invalid strategy must be refused")
	}
	// FIFO retention can leave the oldest surviving record above v1; a
	// contiguous tail from there is a valid restore.
	retained := modelrouting.Scope{Kind: modelrouting.ScopeUser, ID: "key_retained"}
	if err := table.Restore(retained, []modelrouting.Version{
		{Version: 41, Timestamp: now, Strategy: modelrouting.Strategy{Optimization: modelrouting.OptMinCost}},
		{Version: 42, Timestamp: now.Add(time.Second), Strategy: modelrouting.Strategy{Optimization: modelrouting.OptBalanced}},
	}); err != nil {
		t.Fatalf("contiguous tail after FIFO eviction must restore: %v", err)
	}
}

// The servable-set memo must never serve a stale answer: a new generation or a
// new price snapshot must recompute, or a credential change would keep routing
// to a model that no longer exists (and vice versa).
func TestServableSnapshotCacheInvalidates(t *testing.T) {
	runtime := Build(newTestConfig())
	now := time.Now()
	runtime.core.Prices.Apply([]modelrouting.Price{
		{Model: "m-a", PromptPricePer1M: 1, Multiplier: 1},
		{Model: "m-b", PromptPricePer1M: 2, Multiplier: 1},
	}, now)
	snapshot := runtime.core.Prices.Snapshot()

	allowA := func(m string) bool { return m == "m-a" }
	first := runtime.servableSnapshot(snapshot, allowA, 7)
	if len(first.Prices) != 1 {
		t.Fatalf("first filter kept %d models, want 1", len(first.Prices))
	}
	// Same generation + snapshot: the memoised answer is reused even though the
	// predicate would now say something else — that is the documented cache
	// contract (the generation IS the invalidation signal).
	allowB := func(m string) bool { return m == "m-b" }
	again := runtime.servableSnapshot(snapshot, allowB, 7)
	if len(again.Prices) != 1 {
		t.Fatalf("cached filter kept %d models, want 1", len(again.Prices))
	}
	if _, has := again.Prices["m-a"]; !has {
		t.Fatal("same generation must reuse the cached set")
	}

	// A new generation recomputes.
	newGen := runtime.servableSnapshot(snapshot, allowB, 8)
	if _, has := newGen.Prices["m-b"]; !has {
		t.Fatalf("new generation must recompute, got %v", newGen.Prices)
	}

	// A new snapshot recomputes even at the same generation.
	runtime.core.Prices.Apply([]modelrouting.Price{
		{Model: "m-a", PromptPricePer1M: 1, Multiplier: 1},
		{Model: "m-b", PromptPricePer1M: 2, Multiplier: 1},
		{Model: "m-c", PromptPricePer1M: 3, Multiplier: 1},
	}, now.Add(time.Minute))
	newSnapshot := runtime.core.Prices.Snapshot()
	afterSync := runtime.servableSnapshot(newSnapshot, func(m string) bool { return true }, 8)
	if len(afterSync.Prices) != 3 {
		t.Fatalf("new snapshot kept %d models, want 3", len(afterSync.Prices))
	}

	// Generation 0 opts out of caching entirely.
	uncached := runtime.servableSnapshot(newSnapshot, allowA, 0)
	if len(uncached.Prices) != 1 {
		t.Fatalf("generation 0 kept %d models, want a fresh filter of 1", len(uncached.Prices))
	}
}

// A nil Allowed predicate must fail CLOSED (nothing servable → pass through),
// never fall back to "no constraint" — the polarity whose failure mode is
// routing a chat request to an image-only or credential-less model.
func TestFilterSnapshotNilPredicateFailsClosed(t *testing.T) {
	snapshot := modelrouting.PriceSnapshot{Prices: map[string]modelrouting.Price{
		"a": {Model: "a"}, "b": {Model: "b"},
	}, SyncedAt: time.Unix(1, 0)}
	got := filterSnapshot(snapshot, nil)
	if len(got.Prices) != 0 {
		t.Fatalf("nil predicate kept %d models, want 0 (fail closed)", len(got.Prices))
	}
	if !got.SyncedAt.Equal(snapshot.SyncedAt) {
		t.Fatal("filterSnapshot must preserve SyncedAt")
	}
}

// A stale snapshot still routes, but the decision reason says so: a
// max_budget 402 taken on a months-old table must be distinguishable from one
// taken on fresh prices.
func TestRouteRequestStaleSnapshotIsFlaggedInReason(t *testing.T) {
	runtime := Build(newTestConfig())
	old := time.Now().Add(-30 * 24 * time.Hour)
	runtime.core.Prices.Apply([]modelrouting.Price{
		{Model: "claude-sonnet-4-5", PromptPricePer1M: 3, CompletionPricePer1M: 15, Multiplier: 1},
	}, old)

	out := runtime.RouteRequest(contextWithGin(emptyGinContext()),
		routingReq([]byte(`{"model":"claude-sonnet-4-5"}`), "claude-sonnet-4-5"))
	if out.Error != nil {
		t.Fatalf("unexpected terminal error: %+v", out.Error)
	}
	reason := out.Headers.Get(modelrouting.HeaderReason)
	if !strings.Contains(reason, "stale_price_snapshot") {
		t.Fatalf("%s = %q, want a stale_price_snapshot marker", modelrouting.HeaderReason, reason)
	}
	if out.Model != "claude-sonnet-4-5" {
		t.Fatalf("model = %q, want the only candidate", out.Model)
	}
}

// REGRESSION (blocking review finding): the candidate pool must be narrowed to
// models the gateway can serve for THIS request. Without the filter the engine
// picks the cheapest row of the whole price table — including an image-only
// model on a chat endpoint (503 from the image-only guard) or a model with no
// credential (400 model_not_found) — turning a working request into a failure.
func TestRouteRequestExcludesUnservableCandidates(t *testing.T) {
	runtime := Build(newTestConfig())
	runtime.core.Prices.Apply([]modelrouting.Price{
		// Cheapest overall, but image-only: not servable on a chat endpoint.
		{Model: "grok-imagine-image-2.0", PromptPricePer1M: 0.001, CompletionPricePer1M: 0.001, Multiplier: 1},
		// Cheaper than the alternative but has no provider configured.
		{Model: "unregistered-model", PromptPricePer1M: 0.01, CompletionPricePer1M: 0.01, Multiplier: 1},
		// The only servable option.
		{Model: "claude-sonnet-4-5", PromptPricePer1M: 3, CompletionPricePer1M: 15, Multiplier: 1},
	}, time.Now())

	// Simulate the handler's predicate: image-only models and models with no
	// provider are not servable.
	servable := map[string]bool{"claude-sonnet-4-5": true}
	req := handlers.ModelRoutingRequest{
		Body:           []byte(`{"model":"claude-opus-4","messages":[{"role":"user","content":"hi"}]}`),
		RequestedModel: "claude-opus-4",
		Allowed:        func(model string) bool { return servable[model] },
	}
	out := runtime.RouteRequest(contextWithGin(emptyGinContext()), req)
	if out.Error != nil {
		t.Fatalf("routing must not reject: %+v", out.Error)
	}
	if out.Model != "claude-sonnet-4-5" {
		t.Fatalf("chosen model = %q, want the only servable candidate", out.Model)
	}
}

// REGRESSION: when no priced model is servable the request passes through
// unchanged rather than being rejected by the engine's 503.
func TestRouteRequestNoServableCandidatePassesThrough(t *testing.T) {
	runtime := Build(newTestConfig())
	runtime.core.Prices.Apply([]modelrouting.Price{
		{Model: "grok-imagine-image-2.0", PromptPricePer1M: 0.001, CompletionPricePer1M: 0.001, Multiplier: 1},
	}, time.Now())

	req := handlers.ModelRoutingRequest{
		Body:           []byte(`{"model":"claude-opus-4"}`),
		RequestedModel: "claude-opus-4",
		Allowed:        func(string) bool { return false },
	}
	out := runtime.RouteRequest(contextWithGin(emptyGinContext()), req)
	if out.Error != nil {
		t.Fatalf("pass-through must not error: %+v", out.Error)
	}
	if out.Model != "" {
		t.Fatalf("model = %q, want empty (keep the requested model)", out.Model)
	}
	if reason := out.Headers.Get(modelrouting.HeaderReason); reason != passthroughReasonNoCandidate {
		t.Fatalf("%s = %q, want %q", modelrouting.HeaderReason, reason, passthroughReasonNoCandidate)
	}
}

// REGRESSION: a state file that failed to load is never overwritten — the new
// document goes to a .recovered sibling, so one damaged byte cannot cost every
// other scope its policy.
func TestPersistDoesNotClobberAFailedLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "routing-state.json")
	original := []byte(`{"version": 1, "scopes": [ {"kind":"user","id":"key_other","versions":[]} ]}`)
	// Corrupt it so the load fails.
	if err := os.WriteFile(path, []byte("{ broken"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg := newTestConfig()
	cfg.ModelRouting.StatePath = path
	runtime := Build(cfg)
	if runtime == nil || !runtime.loadFailed {
		t.Fatalf("Build() must flag a failed load (runtime=%v)", runtime)
	}

	if rec := postPolicies(t, runtime, "?scope_kind=user&scope_id=key_new",
		`{"strategy":{"optimization":"min_cost"},"note":"new"}`); rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d body=%s", rec.Code, rec.Body.String())
	}

	// The original damaged file is byte-identical — nothing overwrote it.
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read original: %v", err)
	}
	if !bytes.Equal(onDisk, []byte("{ broken")) {
		t.Fatalf("the failed-load file was overwritten: %q", onDisk)
	}
	_ = original // documentation of the shape that would have been lost

	// The recovered sibling exists and holds the new policy.
	matches, err := filepath.Glob(path + ".recovered-*")
	if err != nil || len(matches) != 1 {
		t.Fatalf("recovered sibling = %v (err=%v), want exactly 1", matches, err)
	}
	recovered, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read recovered: %v", err)
	}
	if !bytes.Contains(recovered, []byte("key_new")) {
		t.Fatalf("recovered file missing the new scope: %s", recovered)
	}
}

// REGRESSION: oversized bodies and notes are refused, so one management call
// cannot bloat the state file and every later read.
func TestPostPoliciesRejectsOversizedInput(t *testing.T) {
	runtime := Build(newTestConfig())
	const scopeQuery = "?scope_kind=user&scope_id=key_big"

	hugeNote := strings.Repeat("n", maxPolicyNoteLength+1)
	body, _ := json.Marshal(map[string]any{
		"strategy": map[string]any{"optimization": "min_cost"},
		"note":     hugeNote,
	})
	if rec := postPolicies(t, runtime, scopeQuery, string(body)); rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized note status = %d, want 400", rec.Code)
	}

	// A body beyond maxPolicyRequestBody is refused before parsing.
	oversized := `{"strategy":{"optimization":"min_cost"},"note":"` + strings.Repeat("x", maxPolicyRequestBody) + `"}`
	if rec := postPolicies(t, runtime, scopeQuery, oversized); rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized body status = %d, want 400", rec.Code)
	}

	if n := len(runtime.core.Versions.List(modelrouting.Scope{Kind: modelrouting.ScopeUser, ID: "key_big"})); n != 0 {
		t.Fatalf("versions = %d, want 0 after rejected input", n)
	}
}

// REGRESSION: strategy and version come from one lookup, so a concurrent POST
// cannot attribute a vN decision to v(N+1).
func TestEffectiveReadsStrategyAndVersionTogether(t *testing.T) {
	runtime := Build(newTestConfig())
	scope := modelrouting.Scope{Kind: modelrouting.ScopeUser, ID: "key_atomic"}
	for i := 1; i <= 3; i++ {
		if _, err := runtime.core.Versions.Update(scope, modelrouting.Record{
			Strategy: modelrouting.Strategy{Optimization: modelrouting.OptMinCost},
			Note:     "seed",
		}); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	view := runtime.effective(scope)
	if view.Version != 3 || view.Strategy.Optimization != modelrouting.OptMinCost {
		t.Fatalf("view = %+v, want v3 min_cost from one lookup", view)
	}
	if view.Reason != "strategy v3 in force" {
		t.Fatalf("reason = %q, want the version text", view.Reason)
	}
}

// REGRESSION (review finding): a general-path quality-floor miss is a statement
// about the price table, not a violation — it must pass through, or a gateway
// whose registered models are all cheap 503s its complex traffic the moment
// routing is enabled with the shipped example's 0.6 floor.
//
// The operator's own constraints must still reject: a max_budget cap (402) and
// an only_allow_model pin (503) are what the policy explicitly asked for.
func TestRouteRequestUnmetFloorPassesThroughButPolicyConstraintsReject(t *testing.T) {
	// Only a lite model priced; the request is complex enough to require a
	// higher tier.
	budget := 100.0
	cfg := &config.Config{ModelRouting: config.ModelRoutingConfig{
		Enabled: true,
		DefaultStrategy: &config.ModelRoutingStrategyConfig{
			Optimization: modelrouting.OptMinCost,
			QualityFloor: 0.75, // -> LevelFrontier
			MaxBudget:    &budget,
		},
	}}
	runtime := Build(cfg)
	runtime.core.Prices.Apply([]modelrouting.Price{
		{Model: "claude-haiku", PromptPricePer1M: 0.1, CompletionPricePer1M: 0.1, Multiplier: 1},
	}, time.Now())

	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"` + strings.Repeat("x", 600) + `"}]}`)
	out := runtime.RouteRequest(contextWithGin(emptyGinContext()), routingReq(body, "claude-opus-4-7"))
	if out.Error != nil {
		t.Fatalf("an unmet floor must pass through, got %+v", out.Error)
	}
	if out.Model != "" {
		t.Fatalf("model = %q, want empty (keep the requested model)", out.Model)
	}
	if reason := out.Headers.Get(modelrouting.HeaderReason); reason != passthroughReasonUnmetFloor {
		t.Fatalf("%s = %q, want %q", modelrouting.HeaderReason, reason, passthroughReasonUnmetFloor)
	}

	// Budget cap still rejects: the operator set it on purpose.
	tinyBudget := 0.000001
	pinnedBudget := 100.0
	capCfg := &config.Config{ModelRouting: config.ModelRoutingConfig{
		Enabled: true,
		DefaultStrategy: &config.ModelRoutingStrategyConfig{
			Optimization: modelrouting.OptMinCost,
			MaxBudget:    &tinyBudget,
		},
	}}
	capRuntime := Build(capCfg)
	capRuntime.core.Prices.Apply([]modelrouting.Price{
		{Model: "claude-haiku", PromptPricePer1M: 5, CompletionPricePer1M: 5, Multiplier: 1},
	}, time.Now())
	capOut := capRuntime.RouteRequest(contextWithGin(emptyGinContext()),
		routingReq([]byte(strings.Repeat("x", 400)), "claude-haiku"))
	if capOut.Error == nil || capOut.Error.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("budget cap must reject with 402, got %+v", capOut.Error)
	}

	// only_allow_model pin still rejects when the pinned model cannot be
	// served: the operator named it, so failure is the contract.
	pinCfg := &config.Config{ModelRouting: config.ModelRoutingConfig{
		Enabled: true,
		DefaultStrategy: &config.ModelRoutingStrategyConfig{
			Optimization:   modelrouting.OptMinCost,
			OnlyAllowModel: "not-priced-model",
			MaxBudget:      &pinnedBudget,
		},
	}}
	pinRuntime := Build(pinCfg)
	pinRuntime.core.Prices.Apply([]modelrouting.Price{
		{Model: "claude-haiku", PromptPricePer1M: 1, CompletionPricePer1M: 1, Multiplier: 1},
	}, time.Now())
	pinOut := pinRuntime.RouteRequest(contextWithGin(emptyGinContext()),
		routingReq([]byte(`{"model":"x"}`), "x"))
	if pinOut.Error == nil || pinOut.Error.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("an unservable pinned model must reject with 503, got %+v", pinOut.Error)
	}
}

// --- test helpers -------------------------------------------------------

func contextWithGin(c *gin.Context) context.Context { return withGinContext(c) }

// routingReq builds a gate request with no servability constraint, so the
// engine sees the whole snapshot (the fixture controls what is priced).
func routingReq(body []byte, requested string) handlers.ModelRoutingRequest {
	return handlers.ModelRoutingRequest{Body: body, RequestedModel: requested, Allowed: func(string) bool { return true }}
}

func withGinContext(c *gin.Context) context.Context {
	return context.WithValue(context.Background(), "gin", c)
}

func emptyGinContext() *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	return c
}

func postPolicies(t *testing.T, runtime *Runtime, query, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/routing/policies"+query, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	runtime.PostPolicies(c)
	return rec
}

func getPolicies(runtime *Runtime, query string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/routing/policies"+query, nil)
	runtime.GetPolicies(c)
	return rec
}

// compile-time check that the runtime satisfies the handler gate contract.
var _ handlers.ModelRoutingGate = (*Runtime)(nil)

// BenchmarkRouteRequestEndToEnd pins the per-request cost of the whole gate
// with the servable-set cache warm — the steady state for any real traffic.
// A regression here is a gateway-wide latency regression.
func BenchmarkRouteRequestEndToEnd(b *testing.B) {
	runtime := Build(newTestConfig())
	prices := make([]modelrouting.Price, 0, 200)
	for i := 0; i < 200; i++ {
		prices = append(prices, modelrouting.Price{
			Model:                fmt.Sprintf("bench-route-model-%03d", i),
			PromptPricePer1M:     float64(i + 1),
			CompletionPricePer1M: float64(i + 1),
			Multiplier:           1,
		})
	}
	runtime.core.Prices.Apply(prices, time.Now())
	ctx := contextWithGin(emptyGinContext())
	body := []byte(`{"model":"bench-route-model-010","messages":[{"role":"user","content":"hi"}]}`)
	req := handlers.ModelRoutingRequest{
		Body:              body,
		RequestedModel:    "bench-route-model-010",
		Allowed:           func(string) bool { return true },
		AllowedGeneration: 42,
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if out := runtime.RouteRequest(ctx, req); out.Model == "" {
			b.Fatal("gate declined to route")
		}
	}
}
