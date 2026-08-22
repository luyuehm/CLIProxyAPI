// Package shadow implements the enterprise shadow traffic mirroring engine.
//
// Real production requests are optionally copied (mirrored) to candidate models
// asynchronously at a configurable ratio. The primary response path is never
// blocked: mirror dispatch runs on a bounded goroutine pool with a non-blocking
// enqueue, so the main call chain is unaffected even when the pool saturates
// or the candidate upstream is slow.
package shadow

import (
	"encoding/json"
	"regexp"
	"strings"
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
	// RetentionMinutes is the evaluation record retention TTL.
	// Records older than this are pruned during Push. Zero means no TTL pruning.
	RetentionMinutes int `json:"retention-minutes,omitempty"`
	// RetentionMaxRecords is the hard cap on evaluation records after pruning.
	// Zero means no record cap (ring-buffer capacity still applies).
	RetentionMaxRecords int `json:"retention-max-records,omitempty"`
	// RedactSensitiveFields enables automatic redaction of sensitive fields
	// (API keys, passwords, tokens, emails, etc.) from request bodies before
	// hashing for similarity comparison. The candidate endpoint still receives
	// the original unredacted payload.
	RedactSensitiveFields bool `json:"redact-sensitive-fields,omitempty"`
	// RedactCustomPatterns allows additional regex patterns for custom redaction.
	// Each pattern is applied as a case-insensitive regex to JSON string values.
	RedactCustomPatterns []string `json:"redact-custom-patterns,omitempty"`
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

// RedactSensitiveData redacts known sensitive fields from a JSON request body
// for hashing and storage purposes. The original payload is not modified.
// Patterns: API keys, passwords, tokens, email addresses, phone numbers, IPs, URLs.
func RedactSensitiveData(body []byte, customPatterns []string) []byte {
	if len(body) == 0 {
		return body
	}
	// Parse and re-serialize to redact known sensitive keys in the JSON.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		// Not JSON or parse error — return original body.
		return body
	}
	redactJSONMap(raw, 0, customPatterns)
	out, _ := json.Marshal(raw)
	return out
}

// redactJSONMap recursively redacts sensitive string values in a JSON map.
func redactJSONMap(m map[string]json.RawMessage, depth int, customPatterns []string) {
	if depth > 10 {
		return
	}
	for k, v := range m {
		// Check if this key looks sensitive.
		if isSensitiveKey(k) {
			m[k] = json.RawMessage(`"[REDACTED]"`)
			continue
		}
		// Try to parse as nested map.
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(v, &nested); err == nil && nested != nil {
			redactJSONMap(nested, depth+1, customPatterns)
			// Re-marshal the updated nested map back into the parent.
			b, _ := json.Marshal(nested)
			m[k] = json.RawMessage(b)
			continue
		}
		// Try array.
		var arr []json.RawMessage
		if err := json.Unmarshal(v, &arr); err == nil && arr != nil {
			redactJSONArray(arr, depth+1, customPatterns)
			b, _ := json.Marshal(arr)
			m[k] = json.RawMessage(b)
			continue
		}
		// Try string value.
		var s string
		if err := json.Unmarshal(v, &s); err == nil {
			redacted := redactStringValue(s, customPatterns)
			if redacted != s {
				m[k] = json.RawMessage(`"` + jsonEscape(redacted) + `"`)
			}
		}
	}
}

func redactJSONArray(arr []json.RawMessage, depth int, customPatterns []string) {
	if depth > 10 {
		return
	}
	for i, v := range arr {
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(v, &nested); err == nil && nested != nil {
			redactJSONMap(nested, depth+1, customPatterns)
			b, _ := json.Marshal(nested)
			arr[i] = json.RawMessage(b)
			continue
		}
		var innerArr []json.RawMessage
		if err := json.Unmarshal(v, &innerArr); err == nil && innerArr != nil {
			redactJSONArray(innerArr, depth+1, customPatterns)
			b, _ := json.Marshal(innerArr)
			arr[i] = json.RawMessage(b)
			continue
		}
		var s string
		if err := json.Unmarshal(v, &s); err == nil {
			redacted := redactStringValue(s, customPatterns)
			if redacted != s {
				arr[i] = json.RawMessage(`"` + jsonEscape(redacted) + `"`)
			}
		}
	}
}

// isSensitiveKey checks if a JSON key name indicates sensitive content.
func isSensitiveKey(key string) bool {
	lk := strings.ToLower(key)
	return lk == "api_key" || lk == "apikey" || lk == "api-key" ||
		lk == "password" || lk == "passwd" || lk == "secret" ||
		lk == "token" || lk == "access_token" || lk == "access-token" ||
		strings.HasSuffix(lk, "key") || strings.HasSuffix(lk, "secret") ||
		strings.HasSuffix(lk, "token") || lk == "authorization" ||
		lk == "auth" || lk == "credential" || lk == "credentials" ||
		strings.HasSuffix(lk, "password") || strings.HasSuffix(lk, "api_key")
}

// redactStringValue applies email/phone/IP/URL pattern redaction to a string value.
func redactStringValue(s string, customPatterns []string) string {
	// Email pattern
	reEmail := regexp.MustCompile(`[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}`)
	s = reEmail.ReplaceAllString(s, "[REDACTED-EMAIL]")
	// Phone pattern (basic: digits with optional +()- separators, 7-15 chars)
	rePhone := regexp.MustCompile(`\+?\d[\d\s\-().]{6,14}\d`)
	s = rePhone.ReplaceAllString(s, "[REDACTED-PHONE]")
	// IP address
	reIP := regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)
	s = reIP.ReplaceAllString(s, "[REDACTED-IP]")
	// URL pattern (skip obvious API paths)
	reURL := regexp.MustCompile(`https?://[^\s"']+`)
	s = reURL.ReplaceAllString(s, "[REDACTED-URL]")
	// Custom patterns
	for _, pat := range customPatterns {
		if re, err := regexp.Compile(pat); err == nil {
			s = re.ReplaceAllString(s, "[REDACTED-CUSTOM]")
		}
	}
	return s
}

// jsonEscape escapes a string for safe JSON embedding.
func jsonEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\r", `\r`)
	s = strings.ReplaceAll(s, "\t", `\t`)
	return s
}

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
