package auth

import (
	"context"
	"net/http"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// aliasRoutingExecutor is a no-op executor used to drive selection.
type offlineTestExecutor struct {
	id string
}

func (e *offlineTestExecutor) Identifier() string { return e.id }
func (e *offlineTestExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}
func (e *offlineTestExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}
func (e *offlineTestExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) { return auth, nil }
func (e *offlineTestExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}
func (e *offlineTestExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func setupOfflineSelectionTest(t *testing.T, offlineCfg *internalconfig.Config, localAuth, remoteAuth *Auth, provider, model string) *Manager {
	t.Helper()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetConfig(offlineCfg)
	executor := &offlineTestExecutor{id: provider}
	manager.RegisterExecutor(executor)

	reg := registry.GetGlobalRegistry()
	for _, auth := range []*Auth{localAuth, remoteAuth} {
		if auth == nil {
			continue
		}
		reg.RegisterClient(auth.ID, provider, []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register auth %s: %v", auth.ID, err)
		}
		manager.RefreshSchedulerEntry(auth.ID)
	}
	return manager
}

func TestSelectAuthOfflinePrefersLocal(t *testing.T) {
	const provider = "openai-compatible-offline"
	const model = "hybrid-model"

	offline := &internalconfig.Config{}
	offline.Routing.Offline.Mode = true
	preferLocal := true
	offline.Routing.Offline.PreferLocal = &preferLocal
	allowFallback := false
	offline.Routing.Offline.AllowRemoteFallback = &allowFallback

	localAuth := &Auth{
		ID:       "local-endpoint",
		Provider: provider,
		Status:   StatusActive,
		Attributes: map[string]string{
			"compat_name":         "ollama",
			"provider_key":        provider,
			"base_url":            "http://127.0.0.1:11434/v1",
			AttributeEndpointKind: "local",
		},
	}
	remoteAuth := &Auth{
		ID:       "remote-endpoint",
		Provider: provider,
		Status:   StatusActive,
		Attributes: map[string]string{
			"compat_name":         "openrouter",
			"provider_key":        provider,
			"base_url":            "https://openrouter.ai/api/v1",
			AttributeEndpointKind: "remote",
		},
	}

	manager := setupOfflineSelectionTest(t, offline, localAuth, remoteAuth, provider, model)

	// Offline mode without remote fallback excludes remote and prefers local.
	selected, err := manager.SelectAuth(context.Background(), provider, model, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("SelectAuth() error = %v, want success", err)
	}
	if selected == nil || selected.ID != "local-endpoint" {
		t.Fatalf("SelectAuth() selected = %#v, want local-endpoint", selected)
	}
}

func TestSelectAuthOfflineRemoteExcludedWithoutFallback(t *testing.T) {
	const provider = "openai-compatible-offline-only-remote"
	const model = "remote-only-model"

	offline := &internalconfig.Config{}
	offline.Routing.Offline.Mode = true
	preferLocal := true
	offline.Routing.Offline.PreferLocal = &preferLocal
	allowFallback := false
	offline.Routing.Offline.AllowRemoteFallback = &allowFallback

	remoteAuth := &Auth{
		ID:       "remote-endpoint",
		Provider: provider,
		Status:   StatusActive,
		Attributes: map[string]string{
			"compat_name":         "openrouter",
			"provider_key":        provider,
			"base_url":            "https://openrouter.ai/api/v1",
			AttributeEndpointKind: "remote",
		},
	}

	manager := setupOfflineSelectionTest(t, offline, nil, remoteAuth, provider, model)

	// Only a remote endpoint is configured and remote fallback is off: selection
	// fails with auth unavailable.
	if _, err := manager.SelectAuth(context.Background(), provider, model, cliproxyexecutor.Options{}); err == nil {
		t.Fatal("SelectAuth() = nil error, want auth_unavailable when only remote is configured in offline mode")
	}
}

func TestSelectAuthOfflineRemoteAllowedWithFallback(t *testing.T) {
	const provider = "openai-compatible-offline-fallback"
	const model = "remote-with-fallback"

	offline := &internalconfig.Config{}
	offline.Routing.Offline.Mode = true
	preferLocal := true
	offline.Routing.Offline.PreferLocal = &preferLocal
	allowFallback := true
	offline.Routing.Offline.AllowRemoteFallback = &allowFallback

	remoteAuth := &Auth{
		ID:       "remote-endpoint",
		Provider: provider,
		Status:   StatusActive,
		Attributes: map[string]string{
			"compat_name":         "openrouter",
			"provider_key":        provider,
			"base_url":            "https://openrouter.ai/api/v1",
			AttributeEndpointKind: "remote",
		},
	}

	manager := setupOfflineSelectionTest(t, offline, nil, remoteAuth, provider, model)

	selected, err := manager.SelectAuth(context.Background(), provider, model, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("SelectAuth() error = %v, want success with allow-remote-fallback", err)
	}
	if selected == nil || selected.ID != "remote-endpoint" {
		t.Fatalf("SelectAuth() selected = %#v, want remote-endpoint", selected)
	}
}
