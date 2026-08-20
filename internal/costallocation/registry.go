package costallocation

import (
	"sync"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

var (
	registryMu      sync.RWMutex
	registryTracker *Tracker
)

// Install registers a tracker as a usage plugin on the default usage manager
// and stores it for management API reads.
func Install(t *Tracker) {
	if t == nil {
		return
	}
	registryMu.Lock()
	registryTracker = t
	registryMu.Unlock()
	coreusage.RegisterNamedPlugin("costallocation", t)
}

// Active returns the currently installed tracker, or nil when none is active.
func Active() *Tracker {
	registryMu.RLock()
	defer registryMu.RUnlock()
	if registryTracker == nil || !registryTracker.Enabled() {
		return nil
	}
	return registryTracker
}

// ReportSnapshot returns the current tracker's report, or an empty report.
func ReportSnapshot() AllocationReport {
	t := Active()
	if t == nil {
		return AllocationReport{GeneratedAt: time.Now()}
	}
	return t.Report()
}

// SummarySnapshot returns the current summary report.
func SummarySnapshot() SummaryReport {
	t := Active()
	if t == nil {
		return SummaryReport{GeneratedAt: time.Now()}
	}
	return t.Summary()
}

// ExportSnapshotCSV returns the CSV export.
func ExportSnapshotCSV(opts QueryOptions) ([]byte, error) {
	t := Active()
	if t == nil {
		emptyTracker := NewTracker(AllocationConfig{})
		return emptyTracker.ExportCSV(opts)
	}
	return t.ExportCSV(opts)
}

// DepartmentsSnapshot returns list of departments.
func DepartmentsSnapshot() []DepartmentSummaryItem {
	t := Active()
	if t == nil {
		return nil
	}
	return t.Departments()
}

// Ensure Tracker satisfies coreusage.Plugin.
var _ coreusage.Plugin = (*Tracker)(nil)
