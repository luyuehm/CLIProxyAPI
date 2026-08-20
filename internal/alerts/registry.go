package alerts

import (
	"context"
	"sync"

	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

// Package-level registry, mirroring the redisqueue usage plugin: the service
// installs a single Manager at startup, the usage manager dispatches records
// to it, and the management API reads snapshots from it.

var (
	registryMu  sync.RWMutex
	registryMgr *Manager
)

// Install registers an alert manager on the default usage manager and starts
// its evaluation loop. It replaces any previously installed manager (stopping
// it first) so repeated config reloads do not leak goroutines. Install is safe
// to call with a disabled config: the manager is still registered but ignores
// every usage record.
func Install(cfg Config) {
	cfg = cfg.Normalized()

	registryMu.Lock()
	previous := registryMgr
	manager := NewManager(cfg)
	registryMgr = manager
	registryMu.Unlock()

	if previous != nil {
		previous.Stop()
	}
	coreusage.RegisterNamedPlugin("alerts", manager)
	manager.Start(context.Background())
}

// Stop terminates the currently installed manager, if any.
func Stop() {
	registryMu.RLock()
	manager := registryMgr
	registryMu.RUnlock()
	if manager != nil {
		manager.Stop()
	}
}

// Active returns the currently installed manager, or nil when none has been
// installed.
func Active() *Manager {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return registryMgr
}

// Snapshot is the management API view of the alerts subsystem.
type Snapshot struct {
	Enabled       bool          `json:"enabled"`
	CheckInterval string        `json:"check_interval"`
	Channels      []ChannelKind `json:"channels"`
	Rules         []Rule        `json:"rules"`
	Events        []Event       `json:"events"`
}

// CurrentSnapshot returns the current manager's snapshot, or an empty
// (disabled) snapshot when no manager is installed.
func CurrentSnapshot() Snapshot {
	manager := Active()
	if manager == nil {
		return Snapshot{}
	}
	return manager.Snapshot()
}
