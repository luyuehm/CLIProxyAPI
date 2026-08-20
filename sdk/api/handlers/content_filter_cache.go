package handlers

import (
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/contentfilter"
)

// contentFilterCacheEntry ties a compiled filter to the identity of the SDK
// config it was built from so a hot reload (which replaces the *SDKConfig
// pointer) invalidates the cache automatically.
type contentFilterCacheEntry struct {
	cfg *config.SDKConfig
	f   *contentfilter.Filter
}

type contentFilterStore struct {
	mu    sync.RWMutex
	entry contentFilterCacheEntry
}

func newContentFilterCache() *contentFilterStore {
	return &contentFilterStore{}
}

// get returns the compiled filter for cfg, building it on first use or when the
// config pointer changes. A disabled or empty config yields nil.
func (c *contentFilterStore) get(cfg *config.SDKConfig) *contentfilter.Filter {
	if cfg == nil {
		return nil
	}

	c.mu.RLock()
	if c.entry.cfg == cfg && c.entry.f != nil {
		f := c.entry.f
		c.mu.RUnlock()
		return f
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	// Re-check under the write lock to avoid duplicate work.
	if c.entry.cfg == cfg && c.entry.f != nil {
		return c.entry.f
	}
	f := contentfilter.New(cfg.ContentFilter)
	c.entry = contentFilterCacheEntry{cfg: cfg, f: f}
	return f
}
