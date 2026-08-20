package costallocation

import (
	"sync"

	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

// Package-level registry, mirroring the costbudget pattern: the service
// installs a single Tracker at startup, the usage manager dispatches records
// to it, and the management API reads snapshots from it.

var (
	registryMu      sync.RWMutex
	registryTracker *Tracker
)

// Install registers a tracker as a usage plugin on the default usage manager
// and stores it for later report reads. It is safe to call on a nil tracker
// (treated as a no-op) and to call multiple times across config reloads — the
// latest tracker replaces the prior one.
func Install(t *Tracker) {
	if t == nil {
		return
	}
	registryMu.Lock()
	registryTracker = t
	registryMu.Unlock()
	coreusage.RegisterNamedPlugin("costallocation", t)
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
// management API cost allocation endpoint.
func ReportSnapshot() CostAllocationReport {
	t := Active()
	if t == nil {
		return CostAllocationReport{}
	}
	return t.Report()
}

// PeriodSummarySnapshot returns the per-period breakdown for each department,
// or an empty slice when no tracker is installed.
func PeriodSummarySnapshot() ([]DepartmentPeriodSummary, error) {
	t := Active()
	if t == nil {
		return []DepartmentPeriodSummary{}, nil
	}
	return t.PeriodSummaryReport()
}

// Ensure the Tracker satisfies coreusage.Plugin at compile time.
var _ coreusage.Plugin = (*Tracker)(nil)
