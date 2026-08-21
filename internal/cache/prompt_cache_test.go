package cache

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestPromptCacheKeyDeterministic(t *testing.T) {
	k1 := PromptCacheKey{Model: "gpt-4", Temperature: 0, Messages: `[{"role":"user","content":"hi"}]`}
	k2 := PromptCacheKey{Model: "gpt-4", Temperature: 0, Messages: `[{"role":"user","content":"hi"}]`}
	if k1.Key() != k2.Key() {
		t.Fatal("same key should produce same hash")
	}
}

func TestPromptCacheKeyDifferentModel(t *testing.T) {
	k1 := PromptCacheKey{Model: "gpt-4", Temperature: 0, Messages: `[{"role":"user","content":"hi"}]`}
	k2 := PromptCacheKey{Model: "claude-3", Temperature: 0, Messages: `[{"role":"user","content":"hi"}]`}
	if k1.Key() == k2.Key() {
		t.Fatal("different models should produce different hashes")
	}
}

func TestPromptCacheKeyDifferentTemperature(t *testing.T) {
	k1 := PromptCacheKey{Model: "gpt-4", Temperature: 0, Messages: `[{"role":"user","content":"hi"}]`}
	k2 := PromptCacheKey{Model: "gpt-4", Temperature: 0.5, Messages: `[{"role":"user","content":"hi"}]`}
	if k1.Key() == k2.Key() {
		t.Fatal("different temperatures should produce different hashes")
	}
}

func TestPromptCacheKeyDifferentMessages(t *testing.T) {
	k1 := PromptCacheKey{Model: "gpt-4", Temperature: 0, Messages: `[{"role":"user","content":"hi"}]`}
	k2 := PromptCacheKey{Model: "gpt-4", Temperature: 0, Messages: `[{"role":"user","content":"hello"}]`}
	if k1.Key() == k2.Key() {
		t.Fatal("different messages should produce different hashes")
	}
}

func TestPromptCacheStoreAndGet(t *testing.T) {
	InitPromptCache(&config.PromptCacheConfig{
		Enabled:    true,
		DefaultTTL: "10s",
		MaxSize:    100,
	})
	defer ClearPromptCache()

	ctx := context.Background()
	key := PromptCacheKey{Model: "gpt-4", Temperature: 0, Messages: `[{"role":"user","content":"hello"}]`}
	body := []byte(`{"id":"123","choices":[{"message":{"content":"world"}}]}`)

	SetPromptCache(ctx, key, body)
	got := GetPromptCache(ctx, key)
	if got == nil {
		t.Fatal("expected cached body, got nil")
	}
	if string(got) != string(body) {
		t.Fatalf("expected %q, got %q", string(body), string(got))
	}
}

func TestPromptCacheMiss(t *testing.T) {
	InitPromptCache(&config.PromptCacheConfig{
		Enabled:    true,
		DefaultTTL: "10s",
		MaxSize:    100,
	})
	defer ClearPromptCache()

	ctx := context.Background()
	key := PromptCacheKey{Model: "gpt-4", Temperature: 0, Messages: `[{"role":"user","content":"never stored"}]`}
	got := GetPromptCache(ctx, key)
	if got != nil {
		t.Fatal("expected nil for cache miss")
	}
}

func TestPromptCacheDisabled(t *testing.T) {
	InitPromptCache(&config.PromptCacheConfig{Enabled: false})
	defer ClearPromptCache()

	ctx := context.Background()
	key := PromptCacheKey{Model: "gpt-4", Temperature: 0, Messages: `[{"role":"user","content":"hi"}]`}

	SetPromptCache(ctx, key, []byte(`{"foo":"bar"}`))
	got := GetPromptCache(ctx, key)
	if got != nil {
		t.Fatal("expected nil when cache is disabled")
	}
}

func TestPromptCacheExpiry(t *testing.T) {
	InitPromptCache(&config.PromptCacheConfig{
		Enabled:    true,
		DefaultTTL: "1ms",
		MaxSize:    100,
	})
	defer ClearPromptCache()

	ctx := context.Background()
	key := PromptCacheKey{Model: "gpt-4", Temperature: 0, Messages: `[{"role":"user","content":"expire me"}]`}
	body := []byte(`{"id":"exp"}`)

	SetPromptCache(ctx, key, body)
	// Wait for expiry
	time.Sleep(5 * time.Millisecond)

	got := GetPromptCache(ctx, key)
	if got != nil {
		t.Fatal("expected nil after expiry")
	}
}

func TestPromptCacheEviction(t *testing.T) {
	InitPromptCache(&config.PromptCacheConfig{
		Enabled:    true,
		DefaultTTL: "1h",
		MaxSize:    2,
	})
	defer ClearPromptCache()

	ctx := context.Background()
	k1 := PromptCacheKey{Model: "gpt-4", Temperature: 0, Messages: `[{"role":"user","content":"a"}]`}
	k2 := PromptCacheKey{Model: "gpt-4", Temperature: 0, Messages: `[{"role":"user","content":"b"}]`}
	k3 := PromptCacheKey{Model: "gpt-4", Temperature: 0, Messages: `[{"role":"user","content":"c"}]`}

	SetPromptCache(ctx, k1, []byte(`{"id":"a"}`))
	SetPromptCache(ctx, k2, []byte(`{"id":"b"}`))
	SetPromptCache(ctx, k3, []byte(`{"id":"c"}`))

	// k1 should be evicted (oldest)
	if body := GetPromptCache(ctx, k1); body != nil {
		t.Fatal("expected k1 to be evicted")
	}
	// k2 and k3 should be available
	if body := GetPromptCache(ctx, k2); body == nil {
		t.Fatal("expected k2 to be available")
	}
	if body := GetPromptCache(ctx, k3); body == nil {
		t.Fatal("expected k3 to be available")
	}
}

func TestPromptCacheClear(t *testing.T) {
	InitPromptCache(&config.PromptCacheConfig{
		Enabled:    true,
		DefaultTTL: "1h",
		MaxSize:    100,
	})
	defer ClearPromptCache()

	ctx := context.Background()
	key := PromptCacheKey{Model: "gpt-4", Temperature: 0, Messages: `[{"role":"user","content":"clear me"}]`}
	SetPromptCache(ctx, key, []byte(`{"id":"clear"}`))

	ClearPromptCache()
	got := GetPromptCache(ctx, key)
	if got != nil {
		t.Fatal("expected nil after clear")
	}
}

func TestPromptCacheDelete(t *testing.T) {
	InitPromptCache(&config.PromptCacheConfig{
		Enabled:    true,
		DefaultTTL: "1h",
		MaxSize:    100,
	})
	defer ClearPromptCache()

	ctx := context.Background()
	key := PromptCacheKey{Model: "gpt-4", Temperature: 0, Messages: `[{"role":"user","content":"delete me"}]`}
	SetPromptCache(ctx, key, []byte(`{"id":"del"}`))

	DeletePromptCache(ctx, key)
	got := GetPromptCache(ctx, key)
	if got != nil {
		t.Fatal("expected nil after delete")
	}
}

func TestPromptCacheGroupIsolation(t *testing.T) {
	InitPromptCache(&config.PromptCacheConfig{
		Enabled:    true,
		DefaultTTL: "1h",
		MaxSize:    100,
	})
	defer ClearPromptCache()

	ctx := context.Background()
	claudeKey := PromptCacheKey{Model: "claude-sonnet-4", Temperature: 0, Messages: `[{"role":"user","content":"hi"}]`}
	gptKey := PromptCacheKey{Model: "gpt-4", Temperature: 0, Messages: `[{"role":"user","content":"hi"}]`}

	SetPromptCache(ctx, claudeKey, []byte(`{"claude":true}`))
	SetPromptCache(ctx, gptKey, []byte(`{"gpt":true}`))

	ClearPromptCacheForGroup("claude")
	// Claude should be cleared
	if body := GetPromptCache(ctx, claudeKey); body != nil {
		t.Fatal("expected claude group to be cleared")
	}
	// GPT should still be available
	if body := GetPromptCache(ctx, gptKey); body == nil {
		t.Fatal("expected gpt group to be available")
	}
}

func TestPromptCacheStats(t *testing.T) {
	InitPromptCache(&config.PromptCacheConfig{
		Enabled:    true,
		DefaultTTL: "1h",
		MaxSize:    100,
	})
	defer ClearPromptCache()

	ctx := context.Background()
	key := PromptCacheKey{Model: "gpt-4", Temperature: 0, Messages: `[{"role":"user","content":"stats"}]`}
	body := []byte(`{"id":"stats"}`)

	// Miss first
	_ = GetPromptCache(ctx, key)

	// Set and get
	SetPromptCache(ctx, key, body)
	_ = GetPromptCache(ctx, key)

	stats := GetPromptCacheStats()
	if stats.Hits.Load() < 1 {
		t.Fatal("expected at least 1 hit")
	}
	if stats.SavedBytes.Load() != int64(len(body)) {
		t.Fatalf("expected %d saved bytes, got %d", len(body), stats.SavedBytes.Load())
	}
}

func TestModelGroup(t *testing.T) {
	tests := []struct {
		model string
		want  string
	}{
		{"gpt-4", "gpt"},
		{"claude-sonnet-4-6", "claude"},
		{"gemini-3-pro", "gemini"},
		{"codex-alpha", "codex"},
		{"unknown-model", "other"},
	}
	for _, tt := range tests {
		got := modelGroup(tt.model)
		if got != tt.want {
			t.Errorf("modelGroup(%q) = %q, want %q", tt.model, got, tt.want)
		}
	}
}

func TestHasPromptCache(t *testing.T) {
	InitPromptCache(&config.PromptCacheConfig{Enabled: false})
	SetPromptCacheEnabled(false)
	if HasPromptCache() {
		t.Fatal("expected false when disabled")
	}
	SetPromptCacheEnabled(true)
	if !HasPromptCache() {
		t.Fatal("expected true when enabled")
	}
	SetPromptCacheEnabled(false)
}

func TestInitPromptCache(t *testing.T) {
	InitPromptCache(&config.PromptCacheConfig{
		Enabled: true,
	})
	if !HasPromptCache() {
		t.Fatal("expected cache enabled after init")
	}
	InitPromptCache(nil)
	if HasPromptCache() {
		t.Fatal("expected cache disabled after nil init")
	}
}

func TestCacheTTLForModel(t *testing.T) {
	InitPromptCache(&config.PromptCacheConfig{
		Enabled:    true,
		DefaultTTL: "1h",
		ModelTTLs: map[string]string{
			"claude": "30m",
			"gpt":    "2h",
		},
	})
	defer ClearPromptCache()

	ttl := cacheTTLForModel("claude-sonnet-4")
	if ttl != 30*time.Minute {
		t.Fatalf("expected 30m TTL for claude model, got %v", ttl)
	}

	ttl = cacheTTLForModel("gpt-4")
	if ttl != 2*time.Hour {
		t.Fatalf("expected 2h TTL for gpt model, got %v", ttl)
	}

	ttl = cacheTTLForModel("unknown-model")
	if ttl != time.Hour {
		t.Fatalf("expected 1h default TTL, got %v", ttl)
	}
}
