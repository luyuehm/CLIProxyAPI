// Package config provides configuration management for the CLI Proxy API server.
// It handles loading and parsing YAML configuration files, and provides structured
// access to application settings including server port, authentication directory,
// debug settings, proxy configuration, and API keys.
package config

import (
	"fmt"
	"strings"
)

// SDKConfig represents the application's configuration, loaded from a YAML file.
type SDKConfig struct {
	// ProxyURL is the URL of an optional proxy server to use for outbound requests.
	ProxyURL string `yaml:"proxy-url" json:"proxy-url"`

	// DisableImageGeneration controls whether the built-in image_generation tool is injected/allowed.
	//
	// Supported values:
	//   - false (default): image_generation is enabled everywhere (normal behavior).
	//   - true: image_generation is disabled everywhere. The server stops injecting it, removes it from request payloads,
	//     and returns 404 for /v1/images/generations and /v1/images/edits.
	//   - "chat": disable image_generation injection for all non-images endpoints (e.g. /v1/responses, /v1/chat/completions),
	//     while keeping /v1/images/generations and /v1/images/edits enabled and preserving image_generation there.
	//   - "passthrough": do not modify the tool list on non-images endpoints — keep image_generation if the client
	//     sent it and do not inject it otherwise; on /v1/images/generations and /v1/images/edits behave like "chat".
	DisableImageGeneration DisableImageGenerationMode `yaml:"disable-image-generation" json:"disable-image-generation"`

	// GPTImage2BaseModel sets the base (mainline) model used by the legacy hosted
	// image_generation tool path when a Codex image request is not proxied directly
	// through the Image API.
	//
	// The value must start with "gpt-" (case-insensitive). If empty or invalid, the
	// default base model ("gpt-5.4-mini") is used.
	GPTImage2BaseModel string `yaml:"gpt-image-2-base-model,omitempty" json:"gpt-image-2-base-model,omitempty"`

	// VideoResultAuthCacheTTL controls how long video IDs stay pinned to the credential
	// that created them. Accepts duration strings like "30m" or "3h".
	// Empty or invalid values use the default 3h.
	VideoResultAuthCacheTTL string `yaml:"video-result-auth-cache-ttl,omitempty" json:"video-result-auth-cache-ttl,omitempty"`

	// ForceModelPrefix requires explicit model prefixes (e.g., "teamA/gemini-3-pro-preview")
	// to target prefixed credentials. When false, unprefixed model requests may use prefixed
	// credentials as well.
	ForceModelPrefix bool `yaml:"force-model-prefix" json:"force-model-prefix"`

	// RequestLog enables or disables detailed request logging functionality.
	RequestLog bool `yaml:"request-log" json:"request-log"`

	// CodexOptimizeMultiAgentV2 mirrors the provider-wide runtime setting for API handlers.
	CodexOptimizeMultiAgentV2 bool `yaml:"-" json:"-"`

	// CodexOrphanDelegationCompatibility mirrors the provider-wide runtime setting for API handlers.
	CodexOrphanDelegationCompatibility bool `yaml:"-" json:"-"`

	// ClaudeCode configures Claude Code compatibility behavior.
	ClaudeCode ClaudeCodeConfig `yaml:"claude-code" json:"claude-code"`

	// APIKeys is a list of keys for authenticating clients to this proxy server.
	APIKeys []string `yaml:"api-keys" json:"api-keys"`

	// PassthroughHeaders controls whether upstream response headers are forwarded to downstream clients.
	// Default is false (disabled).
	PassthroughHeaders bool `yaml:"passthrough-headers" json:"passthrough-headers"`

	// Streaming configures server-side streaming behavior (keep-alives and safe bootstrap retries).
	Streaming StreamingConfig `yaml:"streaming" json:"streaming"`

	// NonStreamKeepAliveInterval controls how often blank lines are emitted for non-streaming responses.
	// <= 0 disables keep-alives. Value is in seconds.
	NonStreamKeepAliveInterval int `yaml:"nonstream-keepalive-interval,omitempty" json:"nonstream-keepalive-interval,omitempty"`

	// RoutingTrace configures the routing_trace object attached to non-streaming
	// responses (RIC-3275, OPENROUT-001): which provider actually served the
	// request, whether a fallback switched it, and why.
	RoutingTrace RoutingTraceConfig `yaml:"routing-trace" json:"routing-trace"`

	// ProviderPrefs configures request-level OpenRouter provider preferences
	// (RIC-3352, OPENROUT-001-S2): parsing the top-level `provider` object and
	// ranking the candidate pool on the in-process latency window. Disabled
	// (or an empty block) installs no latency source and no preference
	// parsing, so the request path is byte-identical to the default build.
	ProviderPrefs ProviderPrefsConfig `yaml:"provider-prefs" json:"provider-prefs"`
}

// ProviderPrefsConfig configures the request-level provider-object pipeline.
//
// The pipeline itself (parse → boundary-narrowed pool transform → fallback
// engine allow_fallbacks/max_price → routing_trace provider_prefs_* fields)
// is always on — it is a pure, stateless transform that is a no-op when the
// request carries no `provider` object. Only the in-process latency window
// (the sort:"latency" / preferred_max_latency data source, option A in
// docs/provider-prefs-decision.md) is gated: without it, a latency sort
// degrades to the existing static order and is recorded as
// latency_source_empty in routing_trace, never blocked.
type ProviderPrefsConfig struct {
	// Enabled installs the in-process latency window. Default false: a
	// request whose provider object asks for a latency sort degrades and is
	// recorded, but the gateway measures nothing.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`

	// Window is the sliding-window horizon for the latency samples; 5m when
	// empty. It is how far back a provider's attempt latencies count.
	Window string `yaml:"window,omitempty" json:"window,omitempty"`
}

// ClaudeCodeConfig configures Claude Code compatibility behavior.
type ClaudeCodeConfig struct {
	// DisableCloakingModelList disables model ID cloaking in Anthropic model list responses.
	DisableCloakingModelList bool `yaml:"disable-cloaking-model-list" json:"disable-cloaking-model-list"`
}

// Routing trace detail levels (RFC 2119 wording in docs/routing-trace.md).
const (
	// RoutingTraceModeOff attaches no routing_trace field at all.
	RoutingTraceModeOff = "off"
	// RoutingTraceModeSummary is the default: provider_id, model, fallback_used
	// and switch_reason only — a bounded payload for every caller.
	RoutingTraceModeSummary = "summary"
	// RoutingTraceModeFull additionally discloses candidates, decision_path,
	// attempts (the failed-attempt trail) and latency_ms.
	RoutingTraceModeFull = "full"
)

// DefaultRoutingTraceMode is the mode used when the config block is absent or
// its mode is empty. The summary variant carries enough to answer "which
// provider served this request, and did a fallback switch it?" while keeping
// the response payload small, so the trace is on by default.
const DefaultRoutingTraceMode = RoutingTraceModeSummary

// RoutingTraceConfig configures the routing_trace response object.
//
// The trace is an additive top-level `metadata.routing_trace` object in a
// non-streaming JSON response body; it never changes status codes or error
// bodies, and it does not alter streaming responses (which keep the existing
// X-OpenRouter-Route / X-OpenRouter-Fallback / X-Fallback-Reason headers).
type RoutingTraceConfig struct {
	// Mode selects the disclosure level: "off", "summary" (default) or "full".
	// An unknown value is rejected at config load time.
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty"`

	// FullTraceAPIKeys narrows "full" mode to the listed client API keys.
	// When non-empty, a key that is not listed receives the summary variant, so
	// a free-tier key can be kept on the smaller payload while paid keys get the
	// detailed attempt trail. Empty means every authenticated key follows Mode.
	//
	// Keys are compared verbatim against the inbound Authorization: Bearer,
	// X-Api-Key or X-Goog-Api-Key credential; they are never logged or echoed.
	FullTraceAPIKeys []string `yaml:"full-trace-api-keys,omitempty" json:"full-trace-api-keys,omitempty"`
}

// NormalizedMode returns the configured mode with the default applied. An
// unknown value falls back to the default so a runtime snapshot can never
// break response rendering; config loading rejects unknown values up front.
func (c RoutingTraceConfig) NormalizedMode() string {
	switch mode := normalizeRoutingTraceMode(c.Mode); mode {
	case RoutingTraceModeOff, RoutingTraceModeSummary, RoutingTraceModeFull:
		return mode
	default:
		return DefaultRoutingTraceMode
	}
}

// Validate rejects an unknown mode at config load time.
func (c RoutingTraceConfig) Validate() error {
	switch mode := normalizeRoutingTraceMode(c.Mode); mode {
	case "", RoutingTraceModeOff, RoutingTraceModeSummary, RoutingTraceModeFull:
		return nil
	default:
		return fmt.Errorf("routing-trace.mode %q is invalid: use %q, %q or %q", c.Mode, RoutingTraceModeOff, RoutingTraceModeSummary, RoutingTraceModeFull)
	}
}

func normalizeRoutingTraceMode(mode string) string {
	return strings.ToLower(strings.TrimSpace(mode))
}

// StreamingConfig holds server streaming behavior configuration.
type StreamingConfig struct {
	// KeepAliveSeconds controls how often the server emits SSE heartbeats (": keep-alive\n\n")
	// or WebSocket Ping control frames.
	// <= 0 disables keep-alives. Default is 0.
	KeepAliveSeconds int `yaml:"keepalive-seconds,omitempty" json:"keepalive-seconds,omitempty"`

	// BootstrapRetries controls how many times the server may retry a streaming request before any bytes are sent,
	// to allow auth rotation / transient recovery.
	// <= 0 disables bootstrap retries. Default is 0.
	BootstrapRetries int `yaml:"bootstrap-retries,omitempty" json:"bootstrap-retries,omitempty"`
}
