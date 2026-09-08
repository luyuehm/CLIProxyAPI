package dataratelimit

import (
	"os"
	"testing"
)

func TestSetupDisabledByDefault(t *testing.T) {
	os.Unsetenv(EnvEnabled)
	opt := ServerOption()
	if opt != nil {
		t.Fatal("ServerOption must return nil when disabled")
	}
}

func TestSetupEnabled(t *testing.T) {
	t.Setenv(EnvEnabled, "true")
	opt := ServerOption()
	if opt == nil {
		t.Fatal("ServerOption must return an option when enabled")
	}
}

func TestSetupEnabledWithKeeper(t *testing.T) {
	t.Setenv(EnvEnabled, "true")
	t.Setenv(EnvKeeperURL, "http://127.0.0.1:4320")
	t.Setenv(EnvKeeperKey, "secret")
	opt := ServerOption()
	if opt == nil {
		t.Fatal("ServerOption must return an option when enabled with Keeper")
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

func TestEnvPositiveInt64(t *testing.T) {
	if got := envPositiveInt64("", 7); got != 7 {
		t.Fatalf("empty = %d, want 7", got)
	}
	if got := envPositiveInt64("0", 7); got != 0 {
		t.Fatalf("0 = %d, want 0", got)
	}
	if got := envPositiveInt64("42", 7); got != 42 {
		t.Fatalf("42 = %d, want 42", got)
	}
	if got := envPositiveInt64("-1", 7); got != 7 {
		t.Fatalf("-1 = %d, want fallback 7", got)
	}
	if got := envPositiveInt64("abc", 7); got != 7 {
		t.Fatalf("abc = %d, want fallback 7", got)
	}
}
