package auth

import (
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func newOfflineAuth(endpointKind string) *Auth {
	attrs := map[string]string{}
	if endpointKind != "" {
		attrs[AttributeEndpointKind] = endpointKind
	}
	return &Auth{ID: "auth-" + endpointKind, Provider: "openai-compatible-local", Attributes: attrs}
}

func offlineConfig(mode, preferLocal, allowRemoteFallback bool) *internalconfig.Config {
	cfg := &internalconfig.Config{}
	cfg.Routing.Offline.Mode = mode
	cfg.Routing.Offline.PreferLocal = &preferLocal
	cfg.Routing.Offline.AllowRemoteFallback = &allowRemoteFallback
	return cfg
}

func TestAuthEndpointKind(t *testing.T) {
	if !newOfflineAuth("local").IsLocalEndpoint() {
		t.Fatal("local kind should be local")
	}
	if !newOfflineAuth("remote").IsRemoteEndpoint() {
		t.Fatal("remote kind should be remote")
	}
	if newOfflineAuth("").IsLocalEndpoint() {
		t.Fatal("empty kind should not be local")
	}
	if newOfflineAuth("").IsRemoteEndpoint() {
		t.Fatal("empty kind should not be remote")
	}
}

func TestAuthOfflineEligibilityNotOffline(t *testing.T) {
	cfg := offlineConfig(false, true, false)
	// No filtering outside offline mode.
	localAuth := newOfflineAuth("local")
	remoteAuth := newOfflineAuth("remote")
	for _, a := range []*Auth{localAuth, remoteAuth} {
		res, ok := authOfflineEligibility(a, cfg)
		if !ok {
			t.Fatalf("authOfflineEligibility(%s) excluded outside offline mode", a.EndpointKind())
		}
		if res.boost != 0 {
			t.Fatalf("authOfflineEligibility(%s) boost = %d, want 0 outside offline mode", a.EndpointKind(), res.boost)
		}
	}
}

func TestAuthOfflineEligibilityRemoteBlockedWithoutFallback(t *testing.T) {
	cfg := offlineConfig(true, true, false)
	remoteAuth := newOfflineAuth("remote")
	if _, ok := authOfflineEligibility(remoteAuth, cfg); ok {
		t.Fatal("remote endpoint should be excluded in offline mode without remote fallback")
	}

	// Untagged endpoints stay eligible to avoid breaking legacy configs.
	untaggedAuth := newOfflineAuth("")
	if _, ok := authOfflineEligibility(untaggedAuth, cfg); !ok {
		t.Fatal("untagged endpoint should stay eligible in offline mode")
	}
}

func TestAuthOfflineEligibilityLocalPreferred(t *testing.T) {
	cfg := offlineConfig(true, true, false)
	localAuth := newOfflineAuth("local")
	res, ok := authOfflineEligibility(localAuth, cfg)
	if !ok {
		t.Fatal("local endpoint should be eligible in offline mode")
	}
	if res.boost != 100 {
		t.Fatalf("local boost = %d, want 100", res.boost)
	}
}

func TestAuthOfflineEligibilityRemoteAllowedWithFallback(t *testing.T) {
	cfg := offlineConfig(true, true, true)
	remoteAuth := newOfflineAuth("remote")
	res, ok := authOfflineEligibility(remoteAuth, cfg)
	if !ok {
		t.Fatal("remote endpoint should be eligible with allow-remote-fallback")
	}
	// Remote does not get a local boost.
	if res.boost != 0 {
		t.Fatalf("remote boost = %d, want 0", res.boost)
	}
}

func TestAuthOfflineEligibilityPreferLocalOff(t *testing.T) {
	cfg := offlineConfig(true, false, false)
	localAuth := newOfflineAuth("local")
	res, ok := authOfflineEligibility(localAuth, cfg)
	if !ok {
		t.Fatal("local endpoint should be eligible")
	}
	if res.boost != 0 {
		t.Fatalf("local boost = %d, want 0 when prefer-local is off", res.boost)
	}
}
