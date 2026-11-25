package config

import (
	"strings"
	"testing"
)

func TestRoutingTraceValidateAcceptsKnownModes(t *testing.T) {
	cases := []struct {
		name string
		mode string
	}{
		{name: "unset", mode: ""},
		{name: "off", mode: RoutingTraceModeOff},
		{name: "summary", mode: RoutingTraceModeSummary},
		{name: "full", mode: RoutingTraceModeFull},
		{name: "case insensitive", mode: " FULL "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := (RoutingTraceConfig{Mode: tc.mode}).Validate(); err != nil {
				t.Fatalf("Validate(%q) = %v, want nil", tc.mode, err)
			}
		})
	}
}

func TestRoutingTraceValidateRejectsUnknownMode(t *testing.T) {
	err := (RoutingTraceConfig{Mode: "verbose"}).Validate()
	if err == nil {
		t.Fatal("Validate(verbose) = nil, want an error")
	}
	// The message must name every accepted value so an operator can fix the
	// config without reading source.
	for _, want := range []string{"off", "summary", "full"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err.Error(), want)
		}
	}
}

func TestRoutingTraceNormalizedModeDefaultsToSummary(t *testing.T) {
	if got := (RoutingTraceConfig{}).NormalizedMode(); got != RoutingTraceModeSummary {
		t.Fatalf("unset NormalizedMode() = %q, want %q", got, RoutingTraceModeSummary)
	}
	if got := (RoutingTraceConfig{Mode: " unknown "}).NormalizedMode(); got != RoutingTraceModeSummary {
		t.Fatalf("unknown NormalizedMode() = %q, want %q", got, RoutingTraceModeSummary)
	}
	if got := (RoutingTraceConfig{Mode: " OFF "}).NormalizedMode(); got != RoutingTraceModeOff {
		t.Fatalf("padding/case NormalizedMode() = %q, want %q", got, RoutingTraceModeOff)
	}
}

// The YAML key is part of the public config contract: renaming it silently
// disables an operator's setting, so it is pinned literally.
func TestRoutingTraceConfigParsesFromYAML(t *testing.T) {
	cfg, errParse := ParseConfigBytes([]byte("routing-trace:\n  mode: full\n  full-trace-api-keys:\n    - paid-key\n"))
	if errParse != nil {
		t.Fatalf("ParseConfigBytes() error = %v", errParse)
	}
	if cfg.RoutingTrace.Mode != RoutingTraceModeFull {
		t.Fatalf("mode = %q, want full", cfg.RoutingTrace.Mode)
	}
	if len(cfg.RoutingTrace.FullTraceAPIKeys) != 1 || cfg.RoutingTrace.FullTraceAPIKeys[0] != "paid-key" {
		t.Fatalf("full-trace-api-keys = %v, want [paid-key]", cfg.RoutingTrace.FullTraceAPIKeys)
	}
}

// An invalid mode must fail config loading, not be silently defaulted.
func TestRoutingTraceInvalidModeFailsConfigLoad(t *testing.T) {
	if _, errParse := ParseConfigBytes([]byte("routing-trace:\n  mode: verbose\n")); errParse == nil {
		t.Fatal("ParseConfigBytes() accepted an invalid routing-trace mode")
	}
}
