package openai

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"
)

// chatCompletionsDedupCache is a tiny in-memory TTL cache to prevent short-window
// duplicate non-streaming /v1/chat/completions requests from producing duplicate
// UI output in clients (notably Claude Code).
//
// Scope: non-streaming only. Streaming responses are not cached.
// TTL is intentionally short to minimize incorrect reuse risk.
type chatCompletionsDedupCache struct {
	mu   sync.Mutex
	data map[string]dedupEntry
}

type dedupEntry struct {
	expireAt time.Time
	status   int
	headers  http.Header
	body     []byte
}

func newChatCompletionsDedupCache() *chatCompletionsDedupCache {
	return &chatCompletionsDedupCache{data: make(map[string]dedupEntry)}
}

func (c *chatCompletionsDedupCache) get(key string, now time.Time) (dedupEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.data == nil {
		return dedupEntry{}, false
	}
	e, ok := c.data[key]
	if !ok {
		return dedupEntry{}, false
	}
	if now.After(e.expireAt) {
		delete(c.data, key)
		return dedupEntry{}, false
	}
	return e, true
}

func (c *chatCompletionsDedupCache) set(key string, e dedupEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.data == nil {
		c.data = make(map[string]dedupEntry)
	}
	c.data[key] = e
}

// makeChatCompletionsDedupKey builds a best-effort stable key for short-window dedup.
// Priority:
// 1) x-client-request-id (if present)
// 2) hash(authz + user-agent + body)
func makeChatCompletionsDedupKey(r *http.Request, rawBody []byte) string {
	if r == nil {
		return ""
	}
	// Prefer explicit idempotency-like request id headers when present.
	xcrid := strings.TrimSpace(r.Header.Get("x-client-request-id"))
	if xcrid == "" {
		xcrid = strings.TrimSpace(r.Header.Get("X-Client-Request-Id"))
	}
	if xcrid != "" {
		return "xcrid:" + xcrid
	}

	authz := strings.TrimSpace(r.Header.Get("Authorization"))
	ua := strings.TrimSpace(r.Header.Get("User-Agent"))
	h := sha256.New()
	_, _ = h.Write([]byte(authz))
	_, _ = h.Write([]byte("\n"))
	_, _ = h.Write([]byte(ua))
	_, _ = h.Write([]byte("\n"))
	_, _ = h.Write(rawBody)
	sum := h.Sum(nil)
	return "sha256:" + hex.EncodeToString(sum)
}

func cloneHeaderSubset(h http.Header) http.Header {
	out := make(http.Header)
	if h == nil {
		return out
	}
	// Keep only safe-to-forward informational headers.
	for _, k := range []string{"Content-Type", "X-Cpa-Version", "X-Cpa-Commit", "X-Cpa-Build-Date"} {
		if v := h.Values(k); len(v) > 0 {
			for _, vv := range v {
				out.Add(k, vv)
			}
		}
	}
	return out
}

// small helper to avoid unused import if future edits change usage.
var _ = sha256.Sum256
