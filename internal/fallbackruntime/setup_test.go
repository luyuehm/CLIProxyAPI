package fallbackruntime

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"gopkg.in/yaml.v3"
)

// The config block maps onto the engine policy: providers are copied in order,
// and the operator-facing durations/constraints are carried through. A
// malformed timeout budget is surfaced rather than silently dropped.
func TestConfigToFallbackConfigMapsChainAndConstraints(t *testing.T) {
	src := config.FallbackConfig{
		Enabled:       true,
		Providers:     []config.FallbackProviderConfig{{Provider: "OpenAI"}, {Provider: "anthropic", Model: "claude"}},
		CostLimitUSD:  0.01,
		TimeoutBudget: "30s",
	}
	got := configToFallbackConfig(src)
	if len(got.Providers) != 2 {
		t.Fatalf("providers = %+v, want 2 entries", got.Providers)
	}
	if got.Providers[0].Provider != "OpenAI" || got.Providers[1].Model != "claude" {
		t.Fatalf("providers = %+v, want order and model override preserved", got.Providers)
	}
	if got.CostLimitUSD != 0.01 || got.TimeoutBudget != "30s" {
		t.Fatalf("constraints = cost %v budget %q, want 0.01 / 30s", got.CostLimitUSD, got.TimeoutBudget)
	}
}

// A disabled or empty block installs no option, so the default build keeps
// today's credential-level failover and pays no engine overhead.
func TestServerOptionReturnsNilUnlessEnabledWithChain(t *testing.T) {
	cases := []struct {
		name string
		cfg  *config.Config
		want bool
	}{
		{"nil config", nil, false},
		{"disabled", &config.Config{Fallback: config.FallbackConfig{Enabled: false, Providers: []config.FallbackProviderConfig{{Provider: "a"}}}}, false},
		{"no providers", &config.Config{Fallback: config.FallbackConfig{Enabled: true}}, false},
		{"enabled with chain", &config.Config{Fallback: config.FallbackConfig{Enabled: true, Providers: []config.FallbackProviderConfig{{Provider: "a"}}}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ServerOption(tc.cfg)
			if (got != nil) != tc.want {
				t.Fatalf("ServerOption() == %v (nil=%v), want non-nil=%v", got, got == nil, tc.want)
			}
		})
	}
}

// The YAML block the operator writes parses into the config block: the
// gateway config type must accept the documented fallback keys.
func TestFallbackBlockYAMLUnmarshal(t *testing.T) {
	raw := `
fallback:
  enabled: true
  providers:
    - provider: openai
    - provider: anthropic
      model: claude
  cost-limit-usd: 0.01
  timeout-budget: 30s
`
	cfg := &config.Config{}
	if err := yaml.Unmarshal([]byte(raw), cfg); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if !cfg.Fallback.Enabled || len(cfg.Fallback.Providers) != 2 {
		t.Fatalf("fallback = %+v, want enabled with 2 providers", cfg.Fallback)
	}
	if cfg.Fallback.Providers[1].Model != "claude" {
		t.Fatalf("model override = %q, want claude", cfg.Fallback.Providers[1].Model)
	}
	if !strings.Contains(cfg.Fallback.TimeoutBudget, "30s") {
		t.Fatalf("timeout budget = %q, want 30s", cfg.Fallback.TimeoutBudget)
	}
}
