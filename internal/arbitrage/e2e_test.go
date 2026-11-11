package arbitrage_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/arbitrage"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// RIC-2458 end-to-end acceptance: the live upstream target must follow KEEPER's
// arbitration decision, and must fall back cleanly when the decision expires.
//
// This drives the real BaseAPIHandler → auth.Manager selection path with a real
// (in-memory) auth store and two fake upstreams, then asserts on which upstream
// actually received the request — the observable proof that the decision, not
// the gateway's default pick, chose the target.

type inMemoryAuthStore struct {
	items map[string]*coreauth.Auth
}

func newInMemoryAuthStore() *inMemoryAuthStore {
	return &inMemoryAuthStore{items: make(map[string]*coreauth.Auth)}
}

func (s *inMemoryAuthStore) List(context.Context) ([]*coreauth.Auth, error) {
	out := make([]*coreauth.Auth, 0, len(s.items))
	for _, item := range s.items {
		out = append(out, item)
	}
	return out, nil
}

func (s *inMemoryAuthStore) Save(_ context.Context, auth *coreauth.Auth) (string, error) {
	s.items[auth.ID] = auth
	return auth.ID, nil
}

func (s *inMemoryAuthStore) Delete(_ context.Context, id string) error {
	delete(s.items, id)
	return nil
}

type upstreamRecorder struct {
	server    *httptest.Server
	hits      atomic.Int64
	lastModel atomic.Value
}

func newUpstreamRecorder(t *testing.T) *upstreamRecorder {
	t.Helper()
	recorder := &upstreamRecorder{}
	recorder.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		recorder.hits.Add(1)
		var payload struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &payload)
		recorder.lastModel.Store(payload.Model)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-1","object":"chat.completion","model":"` + payload.Model + `",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}
		}`))
	}))
	t.Cleanup(recorder.server.Close)
	return recorder
}

func newHandlerAndManager(t *testing.T) (*handlers.BaseAPIHandler, *coreauth.Manager) {
	t.Helper()
	manager := coreauth.NewManager(newInMemoryAuthStore(), nil, nil)
	return handlers.NewBaseAPIHandlers(nil, manager), manager
}

func chat(t *testing.T, handler *handlers.BaseAPIHandler, model string) {
	t.Helper()
	if _, _, errMsg := handler.ExecuteWithAuthManager(
		context.Background(),
		"openai",
		model,
		[]byte(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`),
		"",
	); errMsg != nil {
		t.Fatalf("execute failed: %v", errMsg.Error)
	}
}

func TestDecisionDrivesUpstreamTarget(t *testing.T) {
	aliyun := newUpstreamRecorder(t)
	tencent := newUpstreamRecorder(t)

	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	handler, manager := newHandlerAndManager(t)
	lookup := arbitrage.CredentialLookup(func(ref string) (arbitrage.Credential, bool) {
		switch ref {
		case "aliyun-ref":
			return arbitrage.Credential{APIKey: "sk-aliyun"}, true
		case "tencent-ref":
			return arbitrage.Credential{APIKey: "sk-tencent"}, true
		default:
			return arbitrage.Credential{}, false
		}
	})

	// KEEPER selects tencent (cheapest healthy) and delivers its endpoint.
	store := arbitrage.NewStore()
	store.Apply([]arbitrage.Decision{{
		Model: "deepseek-v3", Provider: "tencent",
		Endpoint: tencent.server.URL, AuthRef: "tencent-ref", Reason: "cheapest_healthy",
	}}, now, 30, now, 3)
	reconciler := arbitrage.NewReconciler(manager, store, lookup)
	reconciler.SetEndpointPolicy(arbitrage.EndpointPolicy{AllowInsecure: true}) // httptest upstreams are http://127.0.0.1
	reconciler.Reconcile(context.Background())
	t.Cleanup(reconciler.Release)

	handler.SetModelRouterHost(arbitrage.NewRouterHost(store, clock, func(d arbitrage.Decision) string {
		return d.Provider
	}))

	chat(t, handler, "deepseek-v3")

	if got := tencent.hits.Load(); got != 1 {
		t.Fatalf("tencent upstream hits = %d, want 1 (decision must select it)", got)
	}
	if got := aliyun.hits.Load(); got != 0 {
		t.Fatalf("aliyun upstream hits = %d, want 0", got)
	}

	// KEEPER re-bids to aliyun; the next reconcile must move the live target.
	store.Apply([]arbitrage.Decision{{
		Model: "deepseek-v3", Provider: "aliyun",
		Endpoint: aliyun.server.URL, AuthRef: "aliyun-ref", Reason: "drift_repriced",
	}}, clock(), 30, clock(), 3)
	nextReconciler := arbitrage.NewReconciler(manager, store, lookup)
	nextReconciler.SetEndpointPolicy(arbitrage.EndpointPolicy{AllowInsecure: true})
	nextReconciler.Reconcile(context.Background())
	t.Cleanup(nextReconciler.Release)

	chat(t, handler, "deepseek-v3")

	if got := aliyun.hits.Load(); got != 1 {
		t.Fatalf("aliyun upstream hits = %d, want 1 after the decision changed", got)
	}
	if got := tencent.hits.Load(); got != 1 {
		t.Fatalf("tencent upstream hits = %d, want unchanged at 1", got)
	}
}

// An expired snapshot must fall back to default routing and still serve — never
// fail the request. Here the reconciler's credential is the only one registered,
// so default routing still succeeds; the assertion is that the request is served
// and that the host declines to route.
func TestExpiredDecisionFallsBackWithoutFailure(t *testing.T) {
	tencent := newUpstreamRecorder(t)
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

	handler, manager := newHandlerAndManager(t)
	lookup := arbitrage.CredentialLookup(func(ref string) (arbitrage.Credential, bool) {
		if ref == "tencent-ref" {
			return arbitrage.Credential{APIKey: "sk-tencent"}, true
		}
		return arbitrage.Credential{}, false
	})
	store := arbitrage.NewStore()
	store.Apply([]arbitrage.Decision{{
		Model: "deepseek-v3", Provider: "tencent",
		Endpoint: tencent.server.URL, AuthRef: "tencent-ref",
	}}, now, 30, now, 3)
	reconciler := arbitrage.NewReconciler(manager, store, lookup)
	reconciler.SetEndpointPolicy(arbitrage.EndpointPolicy{AllowInsecure: true}) // httptest upstreams are http://127.0.0.1
	reconciler.Reconcile(context.Background())
	t.Cleanup(reconciler.Release)

	expiredClock := func() time.Time { return now.Add(time.Hour) }
	host := arbitrage.NewRouterHost(store, expiredClock, func(d arbitrage.Decision) string {
		return d.Provider
	})
	handler.SetModelRouterHost(host)

	if host.HasModelRouters() {
		t.Fatal("expired snapshot must not advertise routable models")
	}
	chat(t, handler, "deepseek-v3")

	if got := tencent.hits.Load(); got != 1 {
		t.Fatalf("upstream hits = %d, want 1 (fallback must still serve the request)", got)
	}
}

// The runtime executor must report the raw provider name into usage records, so
// KEEPER's (model, provider) health aggregation sees the arbitrated Provider —
// this is what closes the health feedback loop back into re-bidding.
func TestRuntimeExecutorReportsProviderForUsage(t *testing.T) {
	executor := arbitrage.WrapProviderExecutor("Tencent", nil)
	if got := executor.Identifier(); got != "tencent" {
		t.Fatalf("executor identifier = %q, want tencent (usage_events key by raw provider)", got)
	}
}

// The reconciler must publish the arbitrated model into the global registry so
// auth selection can reach the runtime credential at all.
func TestReconcilerRegistersModelForSelection(t *testing.T) {
	tencent := newUpstreamRecorder(t)
	now := time.Now()
	_, manager := newHandlerAndManager(t)
	lookup := arbitrage.CredentialLookup(func(string) (arbitrage.Credential, bool) {
		return arbitrage.Credential{APIKey: "sk"}, true
	})
	store := arbitrage.NewStore()
	store.Apply([]arbitrage.Decision{{
		Model: "ric2458-probe-model", Provider: "tencent",
		Endpoint: tencent.server.URL, AuthRef: "tencent-ref",
	}}, now, 30, now, 3)
	reconciler := arbitrage.NewReconciler(manager, store, lookup)
	reconciler.SetEndpointPolicy(arbitrage.EndpointPolicy{AllowInsecure: true}) // httptest upstreams are http://127.0.0.1
	reconciler.Reconcile(context.Background())
	t.Cleanup(reconciler.Release)

	if providers := registry.GetGlobalRegistry().GetModelProviders("ric2458-probe-model"); len(providers) == 0 {
		t.Fatal("the arbitrated model must be registered so auth selection can route to it")
	}
}
