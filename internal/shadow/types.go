// Package shadow implements the enterprise shadow traffic mirroring engine.
//
// Real production requests are optionally copied (mirrored) to candidate models
// asynchronously at a configurable ratio. The primary response path is never
// blocked: mirror dispatch runs on a bounded goroutine pool with a non-blocking
// enqueue, so the main call chain is unaffected even when the pool saturates
// or the candidate upstream is slow.
package shadow

import (
	"time"
)

// MirrorConfig configures one shadow mirroring rule.
type MirrorConfig struct {
	// Model is the client-visible production model that is being mirrored.
	Model string `json:"model"`
	// Candidate is the candidate model that receives the mirrored traffic.
	Candidate string `json:"candidate"`
	// Ratio is the sampling ratio in [0,1]. 0.1 mirrors 10% of matching requests.
	Ratio float64 `json:"ratio"`
	// Endpoint is the candidate upstream base URL (OpenAI-compatible /v1).
	// When empty, mirroring falls back to the primary provider endpoint.
	Endpoint string `json:"endpoint,omitempty"`
	// APIKey is the credential used against the candidate endpoint.
	//
	// The value is redacted in JSON marshaling and never logged.
	APIKey string `json:"-"`
	// Headers injects additional headers on mirrored requests.
	Headers map[string]string `json:"headers,omitempty"`
	// UserHeader allows per-request attribution to a department/user for canary routing
	// (e.g. "X-Team-ID"). Mirrored requests carry the same value.
	UserHeader string `json:"user-header,omitempty"`
}

// CanaryConfig configures a canary release for a model pair.
type CanaryConfig struct {
	// Model is the production model.
	Model string `json:"model"`
	// Candidate is the canary candidate model.
	Candidate string `json:"candidate"`
	// Weight is the percentage of matching traffic routed to the candidate, in [0,100].
	Weight int `json:"weight"`
	// Provider is the upstream provider key (e.g. "claude", "openai") used to pick a
	// shadow-mode fast path when mirror is enabled; otherwise ignored.
	Provider string `json:"provider,omitempty"`
	// Headers are headers that force the candidate route when present on the request.
	Headers map[string]string `json:"headers,omitempty"`
	// UserIDHeader names a request header whose value (department/user id) forces
	// the candidate route. Empty means no user-scoped forcing.
	UserIDHeader string `json:"user-id-header,omitempty"`
	// UserIDs lists the department/user values that force the candidate route.
	UserIDs []string `json:"user-ids,omitempty"`
}

// Config is the root shadow-mode configuration.
type Config struct {
	// Enabled toggles the whole shadow engine.
	Enabled bool `json:"enabled"`
	// QueueSize bounds the pending mirror jobs. When full, incoming mirrors are dropped
	// instead of blocking the caller.
	QueueSize int `json:"queue-size,omitempty"`
	// WorkerCount bounds the concurrent mirror evaluations.
	WorkerCount int `json:"worker-count,omitempty"`
	// Timeout bounds one mirror evaluation.
	Timeout time.Duration `json:"-"`
	// Mirrors is the list of shadow mirroring rules.
	Mirrors []MirrorConfig `json:"mirrors,omitempty"`
	// Canaries is the list of canary release rules.
	Canaries []CanaryConfig `json:"canaries,omitempty"`
}

// Defaults fills unset tunables with safe production defaults.
func (c *Config) Defaults() {
	if c.QueueSize <= 0 {
		c.QueueSize = 256
	}
	if c.WorkerCount <= 0 {
		c.WorkerCount = 4
	}
	if c.Timeout <= 0 {
		c.Timeout = 30 * time.Second
	}
}

// MirrorTarget is a single candidate the engine dispatches to.
type MirrorTarget struct {
	// Rule is the matching mirror rule.
	Rule MirrorConfig
	// Body is the copied request payload with the model field rewritten to the candidate.
	Body []byte
	// Path is the request path (e.g. /v1/chat/completions).
	Path string
	// Headers are the copied request headers with the candidate auth applied.
	Headers map[string][]string
}

// EvalKind distinguishes a mirror evaluation from a canary evaluation.
type EvalKind string

const (
	EvalKindMirror EvalKind = "mirror"
	EvalKindCanary EvalKind = "canary"
)

// Record is one evaluation result stored in the ledger.
type Record struct {
	// ID is a stable unique key for the comparison.
	ID string `json:"id"`
	// Kind is mirror or canary.
	Kind EvalKind `json:"kind"`
	// Timestamp is the evaluation creation time.
	Timestamp time.Time `json:"timestamp"`
	// Model is the production model.
	Model string `json:"model"`
	// Candidate is the candidate model.
	Candidate string `json:"candidate"`
	// PromptHash is the request body/message hash.
	PromptHash string `json:"prompt_hash"`
	// Similarity is the response text similarity score in [0,1].
	Similarity float64 `json:"similarity"`
	// TokenRatio is candidate_tokens / primary_tokens.
	TokenRatio float64 `json:"token_ratio"`
	// PrimaryTTFT is the primary time-to-first-token in milliseconds.
	PrimaryTTFT float64 `json:"primary_ttft_ms"`
	// CandidateTTFT is the candidate time-to-first-token in milliseconds.
	CandidateTTFT float64 `json:"candidate_ttft_ms"`
	// PrimaryTokens is the primary output token count.
	PrimaryTokens int `json:"primary_tokens"`
	// CandidateTokens is the candidate output token count.
	CandidateTokens int `json:"candidate_tokens"`
	// LatencyDeltaMs is candidate_ttft - primary_ttft in milliseconds.
	LatencyDeltaMs float64 `json:"latency_delta_ms"`
	// Error records a candidate-side error message when mirrored evaluation failed.
	Error string `json:"error,omitempty"`
}
