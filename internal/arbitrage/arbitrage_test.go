package arbitrage

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// memoryStore is a minimal in-memory auth.Store so tests can use a real
// Manager (and therefore real register/update/remove semantics) without
// touching the filesystem.
type memoryStore struct {
	mu    sync.Mutex
	items map[string]*coreauth.Auth
}

func newMemoryStore() *memoryStore { return &memoryStore{items: make(map[string]*coreauth.Auth)} }

func (s *memoryStore) List(context.Context) ([]*coreauth.Auth, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*coreauth.Auth, 0, len(s.items))
	for _, item := range s.items {
		out = append(out, item)
	}
	return out, nil
}

func (s *memoryStore) Save(_ context.Context, auth *coreauth.Auth) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[auth.ID] = auth
	return auth.ID, nil
}

func (s *memoryStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.items, id)
	return nil
}

func newTestManager(t *testing.T) *coreauth.Manager {
	t.Helper()
	return coreauth.NewManager(newMemoryStore(), nil, nil)
}

func fixedClock(at time.Time) func() time.Time { return func() time.Time { return at } }

// lookupFrom builds a CredentialLookup over a static ref→key table.
func lookupFrom(pairs map[string]string) CredentialLookup {
	return func(ref string) (Credential, bool) {
		key, ok := pairs[strings.TrimSpace(ref)]
		if !ok {
			return Credential{}, false
		}
		return Credential{APIKey: key}, true
	}
}

// --- Store / freshness -------------------------------------------------------

func TestStoreDecisionRequiresFreshSnapshot(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	store := NewStore()
	store.Apply([]Decision{{
		Model: "deepseek-v3", Provider: "aliyun",
		Endpoint: "https://aliyun.example.com", AuthRef: "aliyun-ref",
	}}, now, 30, now, 3)

	if _, ok := store.Decision("deepseek-v3", now); !ok {
		t.Fatal("decision must be available inside the freshness window")
	}
	// 30s interval × factor 3 = 90s. Just inside, then just outside.
	if _, ok := store.Decision("deepseek-v3", now.Add(89*time.Second)); !ok {
		t.Fatal("decision must still be available just before expiry")
	}
	if _, ok := store.Decision("deepseek-v3", now.Add(91*time.Second)); ok {
		t.Fatal("expired snapshot must not be acted on — availability first")
	}
}

func TestStoreEmptyUntilFirstApply(t *testing.T) {
	t.Parallel()
	store := NewStore()
	if store.Usable(time.Now()) {
		t.Fatal("an unverified store must never report itself usable")
	}
	if _, ok := store.Decision("deepseek-v3", time.Now()); ok {
		t.Fatal("no decision may be served before the first successful pull")
	}
}

// A future-dated server stamp must not pin the snapshot as fresh forever.
func TestStoreClampsFutureGeneratedAt(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	store := NewStore()
	// KEEPER claims the snapshot is a day in the future. The stamp must be
	// clamped to our own clock, so freshness is bounded by the normal window
	// rather than by the (bogus) future date.
	store.Apply([]Decision{{Model: "m", Provider: "p"}}, now.Add(24*time.Hour), 30, now, 2)
	if !store.Usable(now.Add(10 * time.Second)) {
		t.Fatal("the snapshot must still be fresh immediately after apply")
	}
	if store.Usable(now.Add(24 * time.Hour)) {
		t.Fatal("a far-future generated_at must not extend the freshness window")
	}
}

// Decisions without a provider are not actionable and must not be cached.
func TestStoreDropsDecisionsWithoutProvider(t *testing.T) {
	t.Parallel()
	now := time.Now()
	store := NewStore()
	store.Apply([]Decision{
		{Model: "m1", Provider: ""},
		{Model: "  ", Provider: "aliyun"},
		{Model: "m2", Provider: "aliyun"},
	}, now, 30, now, 3)
	if got := len(store.All()); got != 1 {
		t.Fatalf("cached decisions = %d, want 1 (only m2 is actionable)", got)
	}
}

// AuthRef must never reach the observability projection.
func TestStoreViewsOmitAuthRef(t *testing.T) {
	t.Parallel()
	now := time.Now()
	store := NewStore()
	store.Apply([]Decision{{Model: "m", Provider: "aliyun", AuthRef: "secret-ref", Endpoint: "https://x"}}, now, 30, now, 3)
	views := store.Views()
	if len(views) != 1 {
		t.Fatalf("views = %d, want 1", len(views))
	}
	encoded, err := json.Marshal(views)
	if err != nil {
		t.Fatalf("marshal views: %v", err)
	}
	if strings.Contains(string(encoded), "secret-ref") {
		t.Fatalf("observability projection leaked AuthRef: %s", encoded)
	}
}

// --- Router host -------------------------------------------------------------

func TestRouterHostRoutesToDrivenProvider(t *testing.T) {
	t.Parallel()
	now := time.Now()
	store := NewStore()
	store.Apply([]Decision{{
		Model: "deepseek-v3", Provider: "aliyun",
		Endpoint: "https://aliyun.example.com", AuthRef: "aliyun-ref", Reason: "cheapest_healthy",
	}}, now, 30, now, 3)

	host := NewRouterHost(store, fixedClock(now), func(d Decision) string { return d.Provider })

	resp, ok := host.RouteModel(context.Background(), pluginapi.ModelRouteRequest{RequestedModel: "deepseek-v3"})
	if !ok || !resp.Handled {
		t.Fatal("a fresh decision must produce a handled route")
	}
	if resp.TargetKind != pluginapi.ModelRouteTargetProvider {
		t.Fatalf("target kind = %s, want provider", resp.TargetKind)
	}
	if resp.Target != "aliyun" {
		t.Fatalf("target = %s, want aliyun", resp.Target)
	}
}

func TestRouterHostDeclinesUnknownModelAndExpiredSnapshot(t *testing.T) {
	t.Parallel()
	now := time.Now()
	store := NewStore()
	store.Apply([]Decision{{Model: "deepseek-v3", Provider: "aliyun"}}, now, 30, now, 3)
	host := NewRouterHost(store, fixedClock(now), func(d Decision) string { return d.Provider })

	if _, ok := host.RouteModel(context.Background(), pluginapi.ModelRouteRequest{RequestedModel: "other-model"}); ok {
		t.Fatal("an unrouted model must fall through to default routing")
	}
	// Same request, later clock: the snapshot has expired → decline.
	expired := NewRouterHost(store, fixedClock(now.Add(5*time.Minute)), func(d Decision) string { return d.Provider })
	if expired.HasModelRouters() {
		t.Fatal("expired snapshot must not advertise routable models")
	}
	if _, ok := expired.RouteModel(context.Background(), pluginapi.ModelRouteRequest{RequestedModel: "deepseek-v3"}); ok {
		t.Fatal("expired snapshot must not route")
	}
}

// A decision whose provider has no local credential must be declined, so the
// request never targets a provider that would fail upstream.
func TestRouterHostDeclinesWithoutCredential(t *testing.T) {
	t.Parallel()
	now := time.Now()
	store := NewStore()
	store.Apply([]Decision{{Model: "deepseek-v3", Provider: "aliyun", AuthRef: "missing-ref"}}, now, 30, now, 3)
	host := NewRouterHost(store, fixedClock(now), func(d Decision) string { return "" })

	if _, ok := host.RouteModel(context.Background(), pluginapi.ModelRouteRequest{RequestedModel: "deepseek-v3"}); ok {
		t.Fatal("routing must be declined when no credential resolves the auth_ref")
	}
}

// --- Puller ------------------------------------------------------------------

func TestPullerSyncsAndAppliesFreshness(t *testing.T) {
	t.Parallel()
	var gotKey, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get(RequestHeaderName)
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"items":[{"model":"deepseek-v3","provider":"aliyun","endpoint":"https://aliyun.example.com","auth_ref":"aliyun-ref","price_per_1m":20}],
			"total":1,
			"generated_at":"2026-11-11T12:00:00Z",
			"interval_seconds":30
		}`))
	}))
	defer server.Close()

	clock := fixedClock(time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC))
	store := NewStore()
	puller := NewPuller(store, PullerOptions{
		KeeperURL:     server.URL,
		ManagementKey: "shared-key",
		Interval:      time.Hour, // loop is not exercised here
		Now:           clock,
	})
	if err := puller.SyncOnce(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if gotKey != "shared-key" {
		t.Fatalf("management key header = %q, want shared-key", gotKey)
	}
	if gotPath != deliveryEndpoint {
		t.Fatalf("path = %s, want %s", gotPath, deliveryEndpoint)
	}
	decision, ok := store.Decision("deepseek-v3", clock())
	if !ok {
		t.Fatal("decision must be available after a successful pull")
	}
	if decision.Endpoint != "https://aliyun.example.com" || decision.AuthRef != "aliyun-ref" {
		t.Fatalf("decision = %+v, want endpoint and auth ref carried through", decision)
	}
	if pulls, failures := puller.Counters(); pulls != 1 || failures != 0 {
		t.Fatalf("counters = %d/%d, want 1 pull 0 failures", pulls, failures)
	}
}

// A failed pull must latch the previous good snapshot, not clear it.
func TestPullerKeepsLastSnapshotOnFailure(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	store := NewStore()
	store.Apply([]Decision{{Model: "m", Provider: "aliyun", AuthRef: "r"}}, now, 30, now, 3)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "keeper down", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	puller := NewPuller(store, PullerOptions{KeeperURL: server.URL, ManagementKey: "k", Now: fixedClock(now)})
	if err := puller.SyncOnce(context.Background()); err == nil {
		t.Fatal("non-2xx pull must report an error")
	}
	if _, ok := store.Decision("m", now); !ok {
		t.Fatal("a failed pull must keep the last good snapshot")
	}
	if _, failures := puller.Counters(); failures != 1 {
		t.Fatalf("failure counter = %d, want 1", failures)
	}
}

// --- Reconciler --------------------------------------------------------------

func TestReconcilerRegistersRuntimeCredentialAndModels(t *testing.T) {
	t.Parallel()
	now := time.Now()
	store := NewStore()
	store.Apply([]Decision{{
		Model: "deepseek-v3", Provider: "aliyun",
		Endpoint: "https://aliyun.example.com", AuthRef: "aliyun-ref",
	}}, now, 30, now, 3)

	manager := newTestManager(t)
	reconciler := NewReconciler(manager, store, lookupFrom(map[string]string{"aliyun-ref": "sk-aliyun"}))
	reconciler.Reconcile(context.Background())

	if reconciler.AppliedCount() != 1 {
		t.Fatalf("applied = %d, want 1", reconciler.AppliedCount())
	}
	authID := runtimeAuthID("deepseek-v3", "aliyun")
	auth, ok := manager.GetByID(authID)
	if !ok {
		t.Fatal("runtime credential was not registered")
	}
	if auth.Provider != "aliyun" {
		t.Fatalf("provider = %s, want aliyun (usage/health keying)", auth.Provider)
	}
	if auth.Attributes["base_url"] != "https://aliyun.example.com" {
		t.Fatalf("base_url = %s, want the decision endpoint", auth.Attributes["base_url"])
	}
	if auth.Attributes[coreauth.AttributeRuntimeOnly] != "true" {
		t.Fatal("runtime credential must be marked runtime_only so it never persists")
	}
	if auth.Metadata != nil {
		t.Fatal("runtime credential must carry no metadata (second persistence guard)")
	}
}

// Repeated reconciles with an unchanged decision must not churn the manager.
func TestReconcilerIsIdempotentForUnchangedDecision(t *testing.T) {
	t.Parallel()
	now := time.Now()
	store := NewStore()
	store.Apply([]Decision{{Model: "m", Provider: "aliyun", Endpoint: "https://a", AuthRef: "r"}}, now, 30, now, 3)
	manager := newTestManager(t)
	reconciler := NewReconciler(manager, store, lookupFrom(map[string]string{"r": "sk"}))

	reconciler.Reconcile(context.Background())
	first, _ := manager.GetByID(runtimeAuthID("m", "aliyun"))

	reconciler.Reconcile(context.Background())
	second, _ := manager.GetByID(runtimeAuthID("m", "aliyun"))
	// An unchanged decision must leave the registered row alone: a re-register
	// would bump RegistrationEpoch and churn the scheduler for nothing.
	if first.RegistrationEpoch != second.RegistrationEpoch {
		t.Fatalf("registration epoch changed on a no-op reconcile: %d → %d", first.RegistrationEpoch, second.RegistrationEpoch)
	}
}

// A retargeted decision removes the old runtime credential.
func TestReconcilerRemovesRetargetedCredential(t *testing.T) {
	t.Parallel()
	now := time.Now()
	store := NewStore()
	store.Apply([]Decision{{Model: "m", Provider: "aliyun", Endpoint: "https://a", AuthRef: "r"}}, now, 30, now, 3)
	manager := newTestManager(t)
	reconciler := NewReconciler(manager, store, lookupFrom(map[string]string{"r": "sk"}))
	reconciler.Reconcile(context.Background())

	oldID := runtimeAuthID("m", "aliyun")
	if _, ok := manager.GetByID(oldID); !ok {
		t.Fatal("setup: expected the aliyun credential")
	}

	// KEEPER re-bids and switches the model to tencent.
	store.Apply([]Decision{{Model: "m", Provider: "tencent", Endpoint: "https://t", AuthRef: "r"}}, now, 30, now, 3)
	reconciler.Reconcile(context.Background())

	if _, ok := manager.GetByID(oldID); ok {
		t.Fatal("the superseded provider credential must be removed")
	}
	newID := runtimeAuthID("m", "tencent")
	if _, ok := manager.GetByID(newID); !ok {
		t.Fatal("the newly selected provider credential must be registered")
	}
}

// An unresolvable auth_ref must never be published as a route.
func TestReconcilerSkipsUnresolvableAuthRef(t *testing.T) {
	t.Parallel()
	now := time.Now()
	store := NewStore()
	store.Apply([]Decision{{Model: "m", Provider: "aliyun", Endpoint: "https://a", AuthRef: "unknown"}}, now, 30, now, 3)
	reconciler := NewReconciler(newTestManager(t), store, lookupFrom(map[string]string{"other": "sk"}))
	reconciler.Reconcile(context.Background())
	if reconciler.AppliedCount() != 0 {
		t.Fatalf("applied = %d, want 0 for an unresolvable auth_ref", reconciler.AppliedCount())
	}
}

// --- Executor wrapper --------------------------------------------------------

// The wrapper must report the raw Provider name so usage_events carry the key
// KEEPER's health aggregation expects.
func TestProviderExecutorReportsRawProviderName(t *testing.T) {
	t.Parallel()
	exec := WrapProviderExecutor("Aliyun", nil)
	if got := exec.Identifier(); got != "aliyun" {
		t.Fatalf("identifier = %q, want aliyun", got)
	}
}

// --- Config gating -----------------------------------------------------------

func TestBuildDisabledByDefault(t *testing.T) {
	t.Setenv(EnvEnabled, "")
	if runtime := Build(RuntimeOptions{Manager: newTestManager(t), Lookup: lookupFrom(nil)}); runtime != nil {
		t.Fatal("the channel must be off unless explicitly enabled")
	}
}

func TestBuildRequiresLookupAndKey(t *testing.T) {
	t.Setenv(EnvEnabled, "true")
	t.Setenv(EnvKeeperURL, "http://keeper:4320")
	t.Setenv(EnvKeeperKey, "shared")
	t.Setenv(EnvAuthRefs, "")
	if runtime := Build(RuntimeOptions{Manager: newTestManager(t)}); runtime != nil {
		t.Fatal("without an auth_ref resolver the channel must stay off (no routing change)")
	}
}

func TestBuildEnabledWithCompleteConfig(t *testing.T) {
	t.Setenv(EnvEnabled, "true")
	t.Setenv(EnvKeeperURL, "http://keeper:4320")
	t.Setenv(EnvKeeperKey, "shared")
	t.Setenv(EnvAuthRefs, "aliyun-ref=sk-aliyun")
	t.Setenv(EnvIntervalSec, "15")
	t.Setenv(EnvStalenessFactor, "2")

	runtime := Build(RuntimeOptions{Manager: newTestManager(t), Lookup: lookupFrom(map[string]string{"aliyun-ref": "sk-aliyun"})})
	if runtime == nil {
		t.Fatal("a fully configured channel must build")
	}
	if runtime.Store == nil || runtime.Puller == nil || runtime.Router == nil || runtime.Reconciler == nil {
		t.Fatal("runtime must be fully assembled")
	}
	if runtime.Puller.opts.Interval != 15*time.Second {
		t.Fatalf("interval = %v, want 15s from env", runtime.Puller.opts.Interval)
	}
	if runtime.Puller.opts.StalenessFactor != 2 {
		t.Fatalf("staleness factor = %d, want 2 from env", runtime.Puller.opts.StalenessFactor)
	}
	// Stop must be safe even though Start was never called.
	runtime.Stop()
}
