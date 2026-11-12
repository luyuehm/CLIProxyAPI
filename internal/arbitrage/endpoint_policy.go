package arbitrage

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
)

// endpointPolicy decides whether a KEEPER-supplied Endpoint may be used as an
// upstream target.
//
// Why this exists: the decision payload is attacker-reachable in the sense that
// whoever can write it (a compromised KEEPER, or a MITM on a plaintext link)
// chooses where the gateway sends the *operator's real provider API key*. Using
// the endpoint unvalidated turns "control the decision feed" into "read any
// internal service with an authenticated request" and into "exfiltrate every
// configured auth_ref key to a host of the attacker's choosing".
//
// The default policy therefore requires https and refuses loopback, private,
// link-local (including cloud metadata 169.254.169.254) and otherwise
// non-public addresses. A dev deployment can opt out explicitly.
type endpointPolicy struct {
	// allowInsecure permits http:// and non-public addresses. Only for local
	// development; never enable it on a reachable deployment.
	allowInsecure bool
	// allowedHosts, when non-empty, restricts endpoints to these hostnames in
	// addition to the address checks.
	allowedHosts map[string]struct{}
}

// defaultEndpointPolicy is the production-safe policy.
func defaultEndpointPolicy() endpointPolicy {
	return endpointPolicy{}
}

// policyFromConfig builds a policy from the operator's environment.
func policyFromConfig(allowInsecure bool, allowedHosts []string) endpointPolicy {
	policy := endpointPolicy{allowInsecure: allowInsecure}
	if len(allowedHosts) > 0 {
		policy.allowedHosts = make(map[string]struct{}, len(allowedHosts))
		for _, host := range allowedHosts {
			host = strings.ToLower(strings.TrimSpace(host))
			if host != "" {
				policy.allowedHosts[host] = struct{}{}
			}
		}
	}
	return policy
}

// validate reports whether endpoint may be used as the upstream base URL.
func (p endpointPolicy) validate(endpoint string) error {
	trimmed := strings.TrimSpace(endpoint)
	if trimmed == "" {
		return fmt.Errorf("endpoint is empty")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return fmt.Errorf("endpoint is not a valid URL: %w", err)
	}
	if parsed.User != nil {
		return fmt.Errorf("endpoint must not embed credentials")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "https" && !(p.allowInsecure && scheme == "http") {
		return fmt.Errorf("endpoint scheme %q is not allowed (https required)", parsed.Scheme)
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return fmt.Errorf("endpoint has no host")
	}
	if len(p.allowedHosts) > 0 {
		if _, ok := p.allowedHosts[host]; !ok {
			return fmt.Errorf("endpoint host %q is not in the configured allowlist", host)
		}
	}
	if p.allowInsecure {
		// Dev mode still refuses the cloud metadata address, which is never a
		// legitimate model provider and is the classic SSRF target.
		if isLinkLocalMetadata(host) {
			return fmt.Errorf("endpoint host %q is a cloud metadata address", host)
		}
		return nil
	}
	if !isPublicHost(host) {
		return fmt.Errorf("endpoint host %q resolves to a non-public address", host)
	}
	return nil
}

// isLinkLocalMetadata reports whether host is the well-known metadata address.
func isLinkLocalMetadata(host string) bool {
	return host == "169.254.169.254" || host == "metadata.google.internal"
}

// isPublicHost reports whether host is a name or address that is safe to reach.
// It refuses literal private/loopback/link-local addresses outright; a public
// hostname is allowed (DNS is resolved at connection time, so a rebinding
// attack is still possible — the allowlist is the mitigation for that).
func isPublicHost(host string) bool {
	if isLinkLocalMetadata(host) {
		return false
	}
	if strings.HasSuffix(host, ".local") || host == "localhost" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return !isNonPublicIP(ip)
	}
	// A bare hostname: it might resolve anywhere, but a private literal is the
	// case we can reject statically. Operators concerned about rebinding should
	// set the allowlist.
	return true
}

// isNonPublicIP reports whether ip is loopback, private, link-local, multicast,
// unspecified or otherwise not a public unicast address.
func isNonPublicIP(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified()
}

// EndpointPolicy is the exported form of the endpoint validation policy, so the
// builder can construct it from configuration without reaching into package
// internals. The zero value is the production-safe policy (https + public
// addresses only), which is also what an unconfigured deployment gets.
type EndpointPolicy struct {
	// AllowInsecure permits http:// and non-public addresses. Development only.
	AllowInsecure bool
	// AllowedHosts, when non-empty, restricts endpoints to these hostnames.
	AllowedHosts []string
}

// policy converts the exported form into the internal validator.
func (p EndpointPolicy) policy() endpointPolicy {
	return policyFromConfig(p.AllowInsecure, p.AllowedHosts)
}

// EndpointPolicyFromEnv builds the policy from the operator's environment:
//
//	CPA_ARBITRAGE_ALLOW_INSECURE_ENDPOINTS=true   (development only)
//	CPA_ARBITRAGE_ALLOWED_ENDPOINT_HOSTS=a.com,b.com
func EndpointPolicyFromEnv() EndpointPolicy {
	return EndpointPolicy{
		AllowInsecure: envBool(EnvAllowInsecureEndpoints),
		AllowedHosts:  splitHostList(os.Getenv(EnvAllowedEndpointHosts)),
	}
}

// envBool parses a boolean environment switch.
func envBool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// splitHostList splits a comma-separated host list, dropping empties.
func splitHostList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if host := strings.TrimSpace(part); host != "" {
			out = append(out, host)
		}
	}
	return out
}
