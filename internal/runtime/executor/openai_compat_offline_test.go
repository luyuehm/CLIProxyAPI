package executor

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestOpenAICompatOfflineEndpointBlocked(t *testing.T) {
	tests := []struct {
		name         string
		offlineMode  bool
		allowRemote  bool
		endpointKind string
		wantBlocked  bool
	}{
		{
			name:         "offline mode blocks remote endpoint",
			offlineMode:  true,
			allowRemote:  false,
			endpointKind: "remote",
			wantBlocked:  true,
		},
		{
			name:         "offline mode allows local endpoint",
			offlineMode:  true,
			allowRemote:  false,
			endpointKind: "local",
			wantBlocked:  false,
		},
		{
			name:         "offline mode with remote fallback allows remote",
			offlineMode:  true,
			allowRemote:  true,
			endpointKind: "remote",
			wantBlocked:  false,
		},
		{
			name:         "offline mode blocks untagged endpoint",
			offlineMode:  true,
			allowRemote:  false,
			endpointKind: "",
			wantBlocked:  true,
		},
		{
			name:         "not offline mode never blocks",
			offlineMode:  false,
			allowRemote:  false,
			endpointKind: "remote",
			wantBlocked:  false,
		},
		{
			name:         "not offline mode allows local",
			offlineMode:  false,
			allowRemote:  false,
			endpointKind: "local",
			wantBlocked:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Routing.Offline.Mode = tt.offlineMode
			cfg.Routing.Offline.AllowRemoteFallback = &tt.allowRemote

			attrs := map[string]string{}
			if tt.endpointKind != "" {
				attrs["endpoint_kind"] = tt.endpointKind
			}
			auth := &cliproxyauth.Auth{Attributes: attrs}

			e := &OpenAICompatExecutor{cfg: cfg}
			got := e.offlineEndpointBlocked(auth)
			if got != tt.wantBlocked {
				t.Fatalf("offlineEndpointBlocked() = %v, want %v", got, tt.wantBlocked)
			}
		})
	}
}
