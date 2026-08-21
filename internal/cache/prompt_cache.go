package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	homekv "github.com/router-for-me/CLIProxyAPI/v7/internal/home"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	log "github.com/sirupsen/logrus"
)

// PromptCacheEntry holds a cached response body with metadata.
type PromptCacheEntry struct {
	Body      []byte `json:"body"`
	Model     string `json:"model"`
	ExpiresAt int64  `json:"expires_at"` // unix nano
	BodyHash  string `json:"body_hash"`
}

// CacheKind indicates whether the cache is in-memory or home-KV backed.
type CacheKind int

const (
	CacheKindMemory CacheKind = iota
	CacheKindHomeKV
)

// PromptCacheStats holds atomic hit/miss/byte counters.
type PromptCacheStats struct {
	Hits        atomic.Int64
	Misses      atomic.Int64
	SavedTokens atomic.Int64
	SavedBytes  atomic.Int64
	Evictions   atomic.Int64
}

// promptCacheInternal is the per-model-group in-memory cache.
type promptCacheInternal struct {
	mu      sync.RWMutex
	entries map[string]PromptCacheEntry
	order   []string // insertion order for expiry sweep
}

// Global prompt cache state.
var (
	promptCacheEnabled atomic.Bool
	promptCacheMu      sync.RWMutex
	promptCaches       map[string]*promptCacheInternal // keyed by model group
	promptCacheCfg     *config.PromptCacheConfig
	promptCacheStats   PromptCacheStats
)

// PromptCacheKey is the composite key for a cached prompt.
type PromptCacheKey struct {
	Model       string  `json:"model"`
	Temperature float64 `json:"temperature"`
	Messages    string  `json:"-"` // raw JSON of messages array
}

// Key returns a stable hex string for this cache key.
func (k PromptCacheKey) Key() string {
	h := sha256.New()
	_, _ = h.Write([]byte(k.Model))
	_, _ = h.Write([]byte{0})
	t := fmt.Sprintf("%.6f", k.Temperature)
	_, _ = h.Write([]byte(t))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(k.Messages))
	return hex.EncodeToString(h.Sum(nil))
}

// PromptCacheStats returns a snapshot of the current counters.
func GetPromptCacheStats() PromptCacheStats {
	return promptCacheStats
}

// HasPromptCache returns true if prompt caching is enabled globally.
func HasPromptCache() bool {
	return promptCacheEnabled.Load()
}

// SetPromptCacheEnabled toggles prompt caching at runtime.
func SetPromptCacheEnabled(enabled bool) {
	promptCacheEnabled.Store(enabled)
}

// InitPromptCache initializes the prompt cache with the given config.
func InitPromptCache(cfg *config.PromptCacheConfig) {
	if cfg == nil {
		promptCacheEnabled.Store(false)
		return
	}
	promptCacheMu.Lock()
	defer promptCacheMu.Unlock()
	promptCacheCfg = cfg
	promptCaches = make(map[string]*promptCacheInternal)
	promptCacheEnabled.Store(cfg.Enabled)
	// Reset counters on re-init
	promptCacheStats.Hits.Store(0)
	promptCacheStats.Misses.Store(0)
	promptCacheStats.SavedTokens.Store(0)
	promptCacheStats.SavedBytes.Store(0)
	promptCacheStats.Evictions.Store(0)
}

// cacheTTLForModel returns the TTL for a given model, falling back to the default.
func cacheTTLForModel(model string) time.Duration {
	promptCacheMu.RLock()
	cfg := promptCacheCfg
	promptCacheMu.RUnlock()
	if cfg == nil {
		return time.Hour
	}
	defaultTTL, _ := time.ParseDuration(cfg.DefaultTTL)
	if defaultTTL <= 0 {
		defaultTTL = time.Hour
	}
	for pattern, raw := range cfg.ModelTTLs {
		if strings.Contains(model, pattern) {
			if d, err := time.ParseDuration(raw); err == nil && d > 0 {
				return d
			}
		}
	}
	return defaultTTL
}

// maxCacheSize returns the maximum entries per model group.
func maxCacheSize() int {
	promptCacheMu.RLock()
	cfg := promptCacheCfg
	promptCacheMu.RUnlock()
	if cfg == nil || cfg.MaxSize <= 0 {
		return 1000
	}
	return cfg.MaxSize
}

// modelGroup returns a cache group key for a model name.
func modelGroup(model string) string {
	lower := strings.ToLower(model)
	for _, prefix := range []string{"claude", "gpt", "gemini", "codex", "grok", "kimi", "qwen", "deepseek"} {
		if strings.Contains(lower, prefix) {
			return prefix
		}
	}
	return "other"
}

// getOrCreateCache returns the cache bucket for a model group, creating it if needed.
func getOrCreateCache(group string) *promptCacheInternal {
	size := maxCacheSize()
	promptCacheMu.Lock()
	defer promptCacheMu.Unlock()
	if c, ok := promptCaches[group]; ok {
		return c
	}
	c := &promptCacheInternal{
		entries: make(map[string]PromptCacheEntry),
		order:   make([]string, 0, size),
	}
	promptCaches[group] = c
	return c
}

// GetPromptCache returns the cached entry for a key, or nil if not found/expired.
func GetPromptCache(ctx context.Context, key PromptCacheKey) []byte {
	if !promptCacheEnabled.Load() {
		return nil
	}
	hash := key.Key()
	model := key.Model

	// Try home KV first
	if client, homeMode, errClient := homekv.CurrentKVClient(); homeMode {
		if errClient != nil {
			log.Debugf("prompt cache: home kv unavailable: %v", errClient)
		} else {
			raw, found, errGet := client.KVGet(ctx, promptCacheKVKey(hash))
			if errGet != nil {
				log.Debugf("prompt cache: home kv get error: %v", errGet)
			} else if found {
				var entry PromptCacheEntry
				if err := json.Unmarshal(raw, &entry); err == nil {
					if time.Now().UnixNano() < entry.ExpiresAt {
						// Sliding expiration: touch TTL
						ttl := time.Until(time.Unix(0, entry.ExpiresAt))
						if ttl > 0 {
							_, _ = client.KVExpire(ctx, promptCacheKVKey(hash), ttl)
						}
						promptCacheStats.Hits.Add(1)
						promptCacheStats.SavedBytes.Add(int64(len(entry.Body)))
						return entry.Body
					}
				}
			}
		}
	}

	// Fall back to in-memory cache
	group := modelGroup(model)
	c := getOrCreateCache(group)
	c.mu.RLock()
	entry, exists := c.entries[hash]
	if !exists || time.Now().UnixNano() >= entry.ExpiresAt {
		c.mu.RUnlock()
		if exists {
			c.mu.Lock()
			delete(c.entries, hash)
			c.mu.Unlock()
		}
		promptCacheStats.Misses.Add(1)
		return nil
	}
	body := entry.Body
	c.mu.RUnlock()
	promptCacheStats.Hits.Add(1)
	promptCacheStats.SavedBytes.Add(int64(len(body)))
	return body
}

// SetPromptCache stores a response body in the cache.
func SetPromptCache(ctx context.Context, key PromptCacheKey, body []byte) {
	if !promptCacheEnabled.Load() || len(body) == 0 {
		return
	}
	hash := key.Key()
	model := key.Model
	ttl := cacheTTLForModel(model)
	expiresAt := time.Now().Add(ttl).UnixNano()

	entry := PromptCacheEntry{
		Body:      body,
		Model:     model,
		ExpiresAt: expiresAt,
	}

	// Try home KV first
	if client, homeMode, errClient := homekv.CurrentKVClient(); homeMode {
		if errClient != nil {
			log.Debugf("prompt cache: home kv set error: %v", errClient)
		} else {
			raw, err := json.Marshal(entry)
			if err == nil {
				written, errSet := client.KVSet(ctx, promptCacheKVKey(hash), raw, homekv.KVSetOptions{EX: ttl})
				if errSet != nil {
					log.Debugf("prompt cache: home kv set error: %v", errSet)
				} else if written {
					return
				}
			}
		}
	}

	// Fall back to in-memory cache
	group := modelGroup(model)
	c := getOrCreateCache(group)
	c.mu.Lock()
	defer c.mu.Unlock()

	entry.Body = cloneBytes(body)
	entry.ExpiresAt = time.Now().Add(ttl).UnixNano()

	// Evict oldest if at capacity
	if len(c.entries) >= maxCacheSize() {
		if _, exists := c.entries[hash]; !exists {
			// Only evict if we're adding a new key
			for i, k := range c.order {
				if _, ok := c.entries[k]; ok {
					delete(c.entries, k)
					c.order = append(c.order[:i], c.order[i+1:]...)
					promptCacheStats.Evictions.Add(1)
					break
				}
			}
		}
	}
	c.entries[hash] = entry
	c.order = append(c.order, hash)
}

// DeletePromptCache removes a specific entry from the cache.
func DeletePromptCache(ctx context.Context, key PromptCacheKey) {
	hash := key.Key()
	model := key.Model

	if client, homeMode, errClient := homekv.CurrentKVClient(); homeMode {
		if errClient == nil {
			_, _ = client.KVDel(ctx, promptCacheKVKey(hash))
		}
	}

	group := modelGroup(model)
	c := getOrCreateCache(group)
	c.mu.Lock()
	delete(c.entries, hash)
	c.mu.Unlock()
}

// ClearPromptCache removes all cached entries.
func ClearPromptCache() {
	promptCacheMu.Lock()
	defer promptCacheMu.Unlock()
	promptCaches = make(map[string]*promptCacheInternal)
}

// ClearPromptCacheForGroup clears all entries for a specific model group.
func ClearPromptCacheForGroup(model string) {
	group := modelGroup(model)
	c := getOrCreateCache(group)
	c.mu.Lock()
	c.entries = make(map[string]PromptCacheEntry)
	c.order = c.order[:0]
	c.mu.Unlock()
}

func promptCacheKVKey(hash string) string {
	return fmt.Sprintf("cpa:prompt-cache:%s", hash)
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
