package config

import (
	"testing"
	"time"
)

func TestOfflineEnabled(t *testing.T) {
	tests := []struct {
		name string
		cfg  *Config
		want bool
	}{
		{"nil config", nil, false},
		{"default empty", &Config{}, false},
		{"mode off", &Config{Routing: RoutingConfig{Offline: OfflineRoutingConfig{Mode: false}}}, false},
		{"mode on", &Config{Routing: RoutingConfig{Offline: OfflineRoutingConfig{Mode: true}}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.OfflineEnabled(); got != tt.want {
				t.Fatalf("OfflineEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOfflinePreferLocalDefault(t *testing.T) {
	// In offline mode, prefer-local defaults to true.
	cfg := &Config{Routing: RoutingConfig{Offline: OfflineRoutingConfig{Mode: true}}}
	if !cfg.OfflinePreferLocal() {
		t.Fatal("OfflinePreferLocal() default = false, want true in offline mode")
	}
	// Explicit false wins.
	explicitFalse := false
	cfg2 := &Config{Routing: RoutingConfig{Offline: OfflineRoutingConfig{Mode: true, PreferLocal: &explicitFalse}}}
	if cfg2.OfflinePreferLocal() {
		t.Fatal("OfflinePreferLocal() = true, want false when explicitly set")
	}
	// Non-offline mode never prefers local.
	cfg3 := &Config{}
	if cfg3.OfflinePreferLocal() {
		t.Fatal("OfflinePreferLocal() = true, want false when offline mode is disabled")
	}
}

func TestOfflineAllowRemoteFallbackDefault(t *testing.T) {
	// In offline mode, remote fallback defaults to false.
	cfg := &Config{Routing: RoutingConfig{Offline: OfflineRoutingConfig{Mode: true}}}
	if cfg.OfflineAllowRemoteFallback() {
		t.Fatal("OfflineAllowRemoteFallback() = true, want false in offline mode")
	}
	// Outside offline mode, remote fallback is the default (nothing filtered).
	cfg2 := &Config{}
	if !cfg2.OfflineAllowRemoteFallback() {
		t.Fatal("OfflineAllowRemoteFallback() = false, want true outside offline mode")
	}
	// Explicit true wins.
	explicitTrue := true
	cfg3 := &Config{Routing: RoutingConfig{Offline: OfflineRoutingConfig{Mode: true, AllowRemoteFallback: &explicitTrue}}}
	if !cfg3.OfflineAllowRemoteFallback() {
		t.Fatal("OfflineAllowRemoteFallback() = false, want true when explicitly set")
	}
}

func TestOfflineHealthCheckConfig(t *testing.T) {
	cfg := &Config{Routing: RoutingConfig{Offline: OfflineRoutingConfig{
		Mode: true,
		HealthCheck: OfflineHealthCheckConfig{
			Enabled:  true,
			Interval: "45s",
			Timeout:  "5s",
			Path:     "/v1/models",
		},
	}}}

	if !cfg.OfflineHealthCheckEnabled() {
		t.Fatal("OfflineHealthCheckEnabled() = false, want true")
	}
	if got := cfg.OfflineHealthCheckInterval(); got != 45*time.Second {
		t.Fatalf("OfflineHealthCheckInterval() = %v, want 45s", got)
	}
	if got := cfg.OfflineHealthCheckTimeout(); got != 5*time.Second {
		t.Fatalf("OfflineHealthCheckTimeout() = %v, want 5s", got)
	}
	if got := cfg.OfflineHealthCheckPath(); got != "/v1/models" {
		t.Fatalf("OfflineHealthCheckPath() = %q, want /v1/models", got)
	}
	if !cfg.OfflineHealthCheckProbeOnlyLocal() {
		t.Fatal("OfflineHealthCheckProbeOnlyLocal() = false, want true in offline mode")
	}
}

func TestOfflineHealthCheckDefaults(t *testing.T) {
	cfg := &Config{Routing: RoutingConfig{Offline: OfflineRoutingConfig{Mode: true, HealthCheck: OfflineHealthCheckConfig{Enabled: true}}}}
	if got := cfg.OfflineHealthCheckInterval(); got != DefaultOfflineHealthCheckInterval {
		t.Fatalf("interval = %v, want default %v", got, DefaultOfflineHealthCheckInterval)
	}
	if got := cfg.OfflineHealthCheckTimeout(); got != DefaultOfflineHealthCheckTimeout {
		t.Fatalf("timeout = %v, want default %v", got, DefaultOfflineHealthCheckTimeout)
	}
	if got := cfg.OfflineHealthCheckPath(); got != DefaultOfflineHealthCheckPath {
		t.Fatalf("path = %q, want default %q", got, DefaultOfflineHealthCheckPath)
	}
	if !cfg.OfflineHealthCheckProbeOnlyLocal() {
		t.Fatal("probe-only-local default = false, want true in offline mode")
	}
}
func TestParseConfigYAMLWithOfflineAndLocal(t *testing.T) {
	yamlBody := `
routing:
  offline:
    mode: true
    prefer-local: false
    allow-remote-fallback: true
    health-check:
      enabled: true
      interval: "45s"
      timeout: "5s"
      path: "/v1/models"
openai-compatibility:
  - name: "ollama"
    base-url: "local://127.0.0.1:11434/v1"
    models:
      - name: "llama3"
        alias: "local-llama"
`
	cfg, err := ParseConfigBytes([]byte(yamlBody))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	if !cfg.OfflineEnabled() {
		t.Fatal("OfflineEnabled() = false, want true")
	}
	if cfg.OfflinePreferLocal() {
		t.Fatal("OfflinePreferLocal() = true, want false (explicit false)")
	}
	if !cfg.OfflineAllowRemoteFallback() {
		t.Fatal("OfflineAllowRemoteFallback() = false, want true (explicit true)")
	}
	if !cfg.OfflineHealthCheckEnabled() {
		t.Fatal("OfflineHealthCheckEnabled() = false, want true")
	}
	if got := cfg.OfflineHealthCheckInterval(); got != 45*time.Second {
		t.Fatalf("interval = %v, want 45s", got)
	}
	if len(cfg.OpenAICompatibility) != 1 {
		t.Fatalf("OpenAICompatibility len = %d, want 1", len(cfg.OpenAICompatibility))
	}
	compat := cfg.OpenAICompatibility[0]
	// local:// base-url is preserved through config parsing; the translation to
	// http:// happens at auth synthesis time.
	if compat.BaseURL != "local://127.0.0.1:11434/v1" {
		t.Fatalf("base-url = %q, want local://127.0.0.1:11434/v1", compat.BaseURL)
	}
}
