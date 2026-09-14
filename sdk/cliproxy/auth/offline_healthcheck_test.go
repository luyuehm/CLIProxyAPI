package auth

import (
	"context"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// probeExecutor is a minimal ProviderExecutor that records the last health-check
// request and returns an OK response.
type probeExecutor struct {
	req *http.Request
}

func (e *probeExecutor) Identifier() string { return "openai-compatible-health-probe" }
func (e *probeExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}
func (e *probeExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}
func (e *probeExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) { return auth, nil }
func (e *probeExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}
func (e *probeExecutor) HttpRequest(_ context.Context, _ *Auth, req *http.Request) (*http.Response, error) {
	e.req = req
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
}

// TestHealthProbeFailureMarksAuthUnavailable exercises the exact state
// transition the health-check loop drives: a probe failure (503) marks the auth
// unavailable through MarkResult with an empty model (auth-level unavailability),
// so selection excludes it; a subsequent success re-admits it.
func TestHealthProbeFailureMarksAuthUnavailable(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	ctx := context.Background()

	auth := &Auth{
		ID:       "health-probe-auth",
		Provider: "openai-compatible-health-probe",
		Status:   StatusActive,
		Attributes: map[string]string{
			"compat_name":  "probe",
			"provider_key": "openai-compatible-probe",
			"base_url":     "http://127.0.0.1:9",
		},
	}
	exec := &probeExecutor{}
	manager.RegisterExecutor(exec)
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	// Drive the probe failure result with an empty model (auth-level unavailability).
	manager.MarkResult(ctx, Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    "",
		Success:  false,
		Error:    &Error{Code: "endpoint_unhealthy", Message: "health check failed", HTTPStatus: http.StatusServiceUnavailable},
	})

	current, ok := manager.GetByID(auth.ID)
	if !ok {
		t.Fatal("auth not found after MarkResult")
	}
	if !current.Unavailable {
		t.Fatal("auth.Unavailable = false after probe failure, want true")
	}

	// A later successful probe clears the unavailability.
	manager.MarkResult(ctx, Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    "",
		Success:  true,
	})

	current, ok = manager.GetByID(auth.ID)
	if !ok {
		t.Fatal("auth not found after success result")
	}
	if current.Unavailable {
		t.Fatal("auth.Unavailable = true after successful probe, want false")
	}
}

// TestHealthProbeURLNormalizedFromLocalScheme verifies that a local:// base URL
// is normalized to http:// when building the probe URL, so the plain HTTP
// transport can reach the local endpoint.
func TestHealthProbeURLNormalizedFromLocalScheme(t *testing.T) {
	normalized := util.NormalizeEndpointBaseURL("local://127.0.0.1:11434/v1")
	if normalized != "http://127.0.0.1:11434/v1" {
		t.Fatalf("NormalizeEndpointBaseURL() = %q, want http://127.0.0.1:11434/v1", normalized)
	}
}
