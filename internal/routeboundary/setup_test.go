package routeboundary

import (
	"testing"
)

func TestSetupDisabledByDefault(t *testing.T) {
	t.Setenv(EnvEnabled, "")
	if rt := Build(nil); rt != nil {
		t.Fatal("Build must return nil when the channel is disabled")
	}
}

func TestSetupEnabledWithoutKeeper(t *testing.T) {
	t.Setenv(EnvEnabled, "true")
	t.Setenv(EnvKeeperURL, "")
	t.Setenv(EnvKeeperKey, "")
	t.Setenv("CPA_BASE_URL", "")
	t.Setenv(EnvManagementKey, "")
	if rt := Build(nil); rt != nil {
		t.Fatal("Build must return nil without a KEEPER URL + key")
	}
}

func TestSetupEnabledWithKeeper(t *testing.T) {
	t.Setenv(EnvEnabled, "true")
	t.Setenv(EnvKeeperURL, "http://127.0.0.1:4320")
	t.Setenv(EnvKeeperKey, "secret")
	rt := Build(nil)
	if rt == nil {
		t.Fatal("Build must return a runtime when enabled with a KEEPER URL + key")
	}
	if rt.Store == nil || rt.Puller == nil || rt.Enforcer == nil || rt.Middleware == nil {
		t.Fatalf("incomplete runtime: %+v", rt)
	}
	// The audit writer is inert without a KEEPER URL, but here it is
	// configured so it must be live.
	if rt.Audit == nil {
		t.Fatal("audit writer must be built when the KEEPER URL is set")
	}
	rt.Stop()
}

func TestSetupManagementKeyFallback(t *testing.T) {
	t.Setenv(EnvEnabled, "true")
	t.Setenv(EnvKeeperURL, "http://127.0.0.1:4320")
	t.Setenv(EnvKeeperKey, "")
	t.Setenv(EnvManagementKey, "shared")
	if rt := Build(nil); rt == nil {
		t.Fatal("the shared management key must satisfy the channel")
	}
}

func TestEnabledFromEnvParsing(t *testing.T) {
	for _, tt := range []struct {
		value string
		want  bool
	}{
		{"", false},
		{"1", true},
		{"true", true},
		{"yes", true},
		{"on", true},
		{"TRUE", true},
		{"0", false},
		{"false", false},
		{"off", false},
		{"garbage", false},
	} {
		t.Run(tt.value, func(t *testing.T) {
			t.Setenv(EnvEnabled, tt.value)
			if got := enabledFromEnv(); got != tt.want {
				t.Fatalf("enabledFromEnv(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}

// TestRuntimeNilOptions: the server options are nil-safe for a disabled
// channel, so the builder's install lines are no-ops by default.
func TestRuntimeNilOptions(t *testing.T) {
	var rt *Runtime
	if opt := rt.MiddlewareServerOption(); opt != nil {
		t.Fatal("a nil runtime must yield a nil middleware option")
	}
	if opt := rt.ExecutionServerOption(); opt != nil {
		t.Fatal("a nil runtime must yield a nil execution option")
	}
	rt.Start()
	rt.Stop() // both no-ops on nil
}
