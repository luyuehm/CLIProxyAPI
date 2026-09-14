package auth

import (
	"strings"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
)

// EndpointKind returns the endpoint tier recorded on the auth (local or remote).
// An empty string means the kind was not tagged; callers treat it as remote-safe.
func (a *Auth) EndpointKind() string {
	if a == nil || a.Attributes == nil {
		return util.EndpointKindUnknown
	}
	return strings.TrimSpace(a.Attributes[AttributeEndpointKind])
}

// IsLocalEndpoint reports whether the auth is tagged as a local (on-prem) endpoint.
func (a *Auth) IsLocalEndpoint() bool {
	return a != nil && a.EndpointKind() == util.EndpointKindLocal
}

// IsRemoteEndpoint reports whether the auth is tagged as a remote endpoint.
func (a *Auth) IsRemoteEndpoint() bool {
	if a == nil {
		return false
	}
	return a.EndpointKind() == util.EndpointKindRemote
}

// authIsOfflineEligible implements offline-first endpoint filtering.
//
// It enforces three rules:
//   - When offline mode is enabled and AllowRemoteFallback is false, remote
//     endpoints are excluded entirely (unless every candidate is remote).
//   - When offline mode is enabled and PreferLocal is true, local endpoints
//     outrank remote endpoints of the same priority tier.
//   - Health-checked authentication entries whose endpoint is unhealthy are
//     excluded, mirroring the request-driven cooldown path.
//
// The function returns a per-auth relative boost value, or a special marker that
// excludes the auth. Callers combine the boost with the configured priority to
// derive the effective selection tier.
func authOfflineEligibility(auth *Auth, cfg *internalconfig.Config) (*authOfflineEligibilityResult, bool) {
	res := &authOfflineEligibilityResult{}
	if auth == nil || cfg == nil {
		return res, true
	}

	local := auth.IsLocalEndpoint()
	remote := auth.IsRemoteEndpoint()

	offlineMode := cfg.OfflineEnabled()
	if !offlineMode {
		// Not in offline mode: no offline filtering. PreferLocal is a no-op
		// unless the health check is enabled.
		return res, true
	}

	allowRemoteFallback := cfg.OfflineAllowRemoteFallback()
	preferLocal := cfg.OfflinePreferLocal()

	// Offline mode ends at the edge: remote endpoints are only usable when the
	// operator explicitly whitelists them as fallback. Pure-remote deployments
	// must opt in with allow-remote-fallback: true.
	if remote && !allowRemoteFallback {
		// A remote endpoint with no explicit kind tag is not "remote"; only a
		// tagged endpoint is excluded. Untagged endpoints stay eligible so the
		// offline mode does not silently break legacy configs that never set
		// endpoint_kind.
		if auth.EndpointKind() != util.EndpointKindUnknown {
			res.exclude = true
			return res, false
		}
	}

	// Local endpoints get a selection boost so they outrank remote endpoints of
	// the same configured priority.
	if local && preferLocal {
		res.boost += 100
	}

	return res, true
}

type authOfflineEligibilityResult struct {
	exclude bool
	boost   int
}
