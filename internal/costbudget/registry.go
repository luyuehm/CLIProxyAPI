package costbudget

import (
	"sync"

	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

// Package-level registry, mirroring the redisqueue usage plugin: the service
// installs a single Tracker at startup, the usage manager dispatches records
// to it, and the management API reads snapshots from it.

var (
	registryMu      sync.RWMutex
	registryTracker *Tracker
)

// Install registers a tracker as a usage plugin on the default usage manager
// and stores it for later report reads. It is safe to call on a nil tracker
// (treated as a no-op) and to call multiple times across config reloads — the
// latest tracker replaces the prior one. The prior tracker is not unregistered
// from the usage manager (the manager dispatches to all registered plugins),
// so Install must only be called when the tracker actually changes to avoid
// duplicate accounting; callers guard this by checking Enabled first.
func Install(t *Tracker) {
	if t == nil {
		return
	}
	registryMu.Lock()
	registryTracker = t
	registryMu.Unlock()
	coreusage.RegisterNamedPlugin("costbudget", t)
}

// Active returns the currently installed tracker, or nil when none has been
// installed (or the installed one is disabled).
func Active() *Tracker {
	registryMu.RLock()
	defer registryMu.RUnlock()
	if registryTracker == nil || !registryTracker.Enabled() {
		return nil
	}
	return registryTracker
}

// ReportSnapshot returns the current tracker's report, or an empty (disabled)
// report when no tracker is installed. It is the entry point for the
// management API budget endpoint.
func ReportSnapshot() BudgetReport {
	t := Active()
	if t == nil {
		return BudgetReport{}
	}
	return t.Report()
}

// Ensure the Tracker satisfies coreusage.Plugin at compile time.
var _ coreusage.Plugin = (*Tracker)(nil)
