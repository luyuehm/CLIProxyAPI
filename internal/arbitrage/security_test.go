package arbitrage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// RIC-2458 security regressions. Each test pins a documented failure mode so it
// cannot silently come back.

// A redirecting control plane must not receive the shared management key. Go
// copies custom headers across a redirect, so the pull client refuses them.
func TestPullerRefusesRedirects(t *testing.T) {
	t.Parallel()
	var leaked atomic.Bool
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(RequestHeaderName) != "" {
			leaked.Store(true)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer attacker.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL, http.StatusFound)
	}))
	defer redirector.Close()

	store := NewStore()
	puller := NewPuller(store, PullerOptions{KeeperURL: redirector.URL, ManagementKey: "super-secret"})
	if err := puller.SyncOnce(context.Background()); err == nil {
		t.Fatal("a redirect must be reported as a pull failure")
	}
	if leaked.Load() {
		t.Fatal("the shared management key was forwarded to the redirect target")
	}
	if store.Usable(time.Now()) {
		t.Fatal("a redirected pull must not populate the store")
	}
}

// A huge advertised interval must not pin the snapshot as fresh forever.
func TestStoreClampsHugeInterval(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)
	store := NewStore()
	store.Apply([]Decision{{Model: "m", Provider: "aliyun"}}, now, 1e12, now, 3)

	// Whatever the clamp lands on, a day later it must not still be fresh.
	if store.Usable(now.Add(24 * time.Hour)) {
		t.Fatal("an oversized interval_seconds must not extend freshness beyond the cap")
	}
	if !store.Usable(now.Add(time.Second)) {
		t.Fatal("a fresh snapshot must still be usable immediately after apply")
	}
}

// An oversized payload is truncated rather than accepted wholesale.
func TestStoreTruncatesOversizedSnapshots(t *testing.T) {
	t.Parallel()
	now := time.Now()
	items := make([]Decision, 0, maxDecisionItems+100)
	for i := 0; i < maxDecisionItems+100; i++ {
		items = append(items, Decision{Model: itoa(i), Provider: "aliyun"})
	}
	store := NewStore()
	store.Apply(items, now, 30, now, 3)
	if got := len(store.All()); got > maxDecisionItems {
		t.Fatalf("cached decisions = %d, want at most %d", got, maxDecisionItems)
	}
}

// Endpoint validation: the delivered endpoint decides where the operator's real
// provider key is sent, so non-public targets must be refused by default.
func TestEndpointPolicyRejectsUnsafeTargets(t *testing.T) {
	t.Parallel()
	policy := defaultEndpointPolicy()

	rejected := []string{
		"",                                     // empty
		"ftp://aliyun.example.com",             // wrong scheme
		"http://aliyun.example.com",            // plaintext
		"https://169.254.169.254/latest",       // cloud metadata
		"https://metadata.google.internal",     // cloud metadata hostname
		"https://127.0.0.1/v1",                 // loopback
		"https://10.0.0.5/v1",                  // private
		"https://192.168.1.10/v1",              // private
		"https://[::1]/v1",                     // loopback v6
		"https://localhost/v1",                 // loopback name
		"https://user:pass@aliyun.example.com", // embedded credentials
		"https:///v1",                          // no host
	}
	for _, endpoint := range rejected {
		if err := policy.validate(endpoint); err == nil {
			t.Errorf("endpoint %q must be rejected", endpoint)
		}
	}

	if err := policy.validate("https://aliyun.example.com/v1"); err != nil {
		t.Fatalf("a public https endpoint must be accepted: %v", err)
	}
}

// Dev mode relaxes https/private but still refuses the metadata address.
func TestEndpointPolicyDevModeStillRefusesMetadata(t *testing.T) {
	t.Parallel()
	policy := policyFromConfig(true, nil)
	if err := policy.validate("http://127.0.0.1:8080/v1"); err != nil {
		t.Fatalf("dev mode must allow loopback: %v", err)
	}
	if err := policy.validate("http://169.254.169.254/latest"); err == nil {
		t.Fatal("dev mode must still refuse the cloud metadata address")
	}
}

// An allowlist restricts endpoints to the named hosts.
func TestEndpointPolicyAllowlist(t *testing.T) {
	t.Parallel()
	policy := policyFromConfig(false, []string{"aliyun.example.com"})
	if err := policy.validate("https://aliyun.example.com/v1"); err != nil {
		t.Fatalf("allowlisted host must be accepted: %v", err)
	}
	if err := policy.validate("https://tencent.example.com/v1"); err == nil {
		t.Fatal("a host outside the allowlist must be rejected")
	}
}

// A delivered endpoint that fails validation must never get a credential.
func TestReconcilerRefusesUnsafeEndpoint(t *testing.T) {
	t.Parallel()
	now := time.Now()
	store := NewStore()
	store.Apply([]Decision{{
		Model: "m", Provider: "aliyun",
		Endpoint: "https://169.254.169.254/latest/meta-data", AuthRef: "r",
	}}, now, 30, now, 3)

	manager := newTestManager(t)
	reconciler := NewReconciler(manager, store, lookupFrom(map[string]string{"r": "sk-secret"}))
	reconciler.Reconcile(context.Background())

	if reconciler.AppliedCount() != 0 {
		t.Fatalf("applied = %d, want 0: an unsafe endpoint must not receive a credential", reconciler.AppliedCount())
	}
	if len(manager.List()) != 0 {
		t.Fatal("no credential may be registered for a rejected endpoint")
	}
}

// Custom headers must use the gateway's "header:" convention, and must refuse
// names that would override request identity or allow header injection.
func TestCustomHeaderAttribute(t *testing.T) {
	t.Parallel()
	if got, ok := customHeaderAttribute("X-Provider-Tenant"); !ok || got != "header:X-Provider-Tenant" {
		t.Fatalf("attribute = %q ok=%v, want header:X-Provider-Tenant", got, ok)
	}
	for _, name := range []string{"", "  ", "Authorization", "host", "X-Bad\r\nInjected: 1", "X:Y"} {
		if _, ok := customHeaderAttribute(name); ok {
			t.Errorf("header name %q must be refused", name)
		}
	}
}

// The status projection must never expose an auth_ref or a key.
func TestStatusHandlerOmitsSecrets(t *testing.T) {
	t.Parallel()
	now := time.Now()
	store := NewStore()
	store.Apply([]Decision{{
		Model: "m", Provider: "aliyun",
		Endpoint: "https://aliyun.example.com", AuthRef: "tenant-secret-ref",
	}}, now, 30, now, 3)
	runtime := &Runtime{Store: store, Reconciler: NewReconciler(newTestManager(t), store, lookupFrom(nil))}

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, StatusRoutePath, nil)
	runtime.StatusHandler()(c)

	body := recorder.Body.String()
	if strings.Contains(body, "tenant-secret-ref") {
		t.Fatalf("status endpoint leaked auth_ref: %s", body)
	}
	if strings.Contains(body, "sk-") {
		t.Fatalf("status endpoint leaked key material: %s", body)
	}
}
