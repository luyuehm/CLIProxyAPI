package executor

import (
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// TestClaudeCredsBaseURLMetadataFallback pins the managed-credential channel:
// the three managed stores (gitstore/objectstore/postgresstore) populate
// auth.Metadata["base_url"] (via NormalizeCredentialMetadata) but never write
// Attributes["base_url"], so the executor must fall back to Metadata the same
// way resolveCustomAntigravityBaseURL does. Without this, a configured custom
// endpoint on a managed credential is silently dropped.
func TestClaudeCredsBaseURLMetadataFallback(t *testing.T) {
	tests := []struct {
		name    string
		auth    *cliproxyauth.Auth
		wantKey string
		wantURL string
	}{
		{
			name: "nil auth",
			auth: nil,
		},
		{
			name: "metadata base_url only",
			auth: &cliproxyauth.Auth{
				Attributes: map[string]string{"api_key": "sk-ant-oat-meta"},
				Metadata:   map[string]any{"base_url": "https://custom-claude.example.com"},
			},
			wantKey: "sk-ant-oat-meta",
			wantURL: "https://custom-claude.example.com",
		},
		{
			name: "attributes base_url wins over metadata",
			auth: &cliproxyauth.Auth{
				Attributes: map[string]string{"api_key": "sk-ant-oat-attr", "base_url": "https://attr.example.com"},
				Metadata:   map[string]any{"base_url": "https://meta.example.com"},
			},
			wantKey: "sk-ant-oat-attr",
			wantURL: "https://attr.example.com",
		},
		{
			name: "metadata base_url with api_key from metadata",
			auth: &cliproxyauth.Auth{
				Metadata: map[string]any{"base_url": "https://meta-key.example.com", "access_token": "sk-ant-oat-meta-key"},
			},
			wantKey: "sk-ant-oat-meta-key",
			wantURL: "https://meta-key.example.com",
		},
		{
			name: "empty metadata base_url leaves url empty",
			auth: &cliproxyauth.Auth{
				Attributes: map[string]string{"api_key": "sk-ant-oat-empty"},
				Metadata:   map[string]any{},
			},
			wantKey: "sk-ant-oat-empty",
		},
		{
			name: "non-string metadata base_url ignored",
			auth: &cliproxyauth.Auth{
				Attributes: map[string]string{"api_key": "sk-ant-oat-nonstring"},
				Metadata:   map[string]any{"base_url": 42},
			},
			wantKey: "sk-ant-oat-nonstring",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotKey, gotURL := claudeCreds(tt.auth)
			if gotKey != tt.wantKey {
				t.Errorf("claudeCreds() apiKey = %q, want %q", gotKey, tt.wantKey)
			}
			if gotURL != tt.wantURL {
				t.Errorf("claudeCreds() baseURL = %q, want %q", gotURL, tt.wantURL)
			}
		})
	}
}
