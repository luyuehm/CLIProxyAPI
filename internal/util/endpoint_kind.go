package util

import "strings"

// Endpoint kinds for offline-first hybrid routing (D4).
const (
	// EndpointKindLocal marks an on-prem/offline endpoint that does not require
	// an external network.
	EndpointKindLocal = "local"
	// EndpointKindRemote marks a cloud/public endpoint.
	EndpointKindRemote = "remote"
	// EndpointKindUnknown is the fallback when no kind is recorded.
	EndpointKindUnknown = ""
)

// localURIScheme is the `local://` prefix accepted on OpenAI-compatible base
// URLs. It is translated to the plain HTTP scheme at auth-synthesis time and is
// recorded as EndpointKindLocal on the auth attributes.
const localURIScheme = "local://"

// IsLocalEndpointURL reports whether a base URL carries the local:// scheme.
func IsLocalEndpointURL(baseURL string) bool {
	return strings.HasPrefix(strings.TrimSpace(baseURL), localURIScheme)
}

// NormalizeEndpointBaseURL translates local:// to http:// while preserving the
// rest of the URL. Non-local URLs are returned unchanged. The result is never
// a local:// URL, so the normal HTTP transport can consume it unchanged.
func NormalizeEndpointBaseURL(baseURL string) string {
	baseURL = strings.TrimSpace(baseURL)
	if !IsLocalEndpointURL(baseURL) {
		return baseURL
	}
	return "http://" + strings.TrimPrefix(baseURL, localURIScheme)
}
