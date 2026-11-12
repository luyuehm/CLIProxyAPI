package modelrouting

import (
	"sync"
	"time"
)

// Management is the read-only management API over a scope's strategy version
// history and the daily price cache. It is deliberately framework-free so the
// package stays a leaf and can be unit-tested without an HTTP server; the
// Stage 2 decision engine (or a future HTTP management layer) mounts these
// methods behind auth.
//
// It covers the ticket's "management read API" requirement: list versions
// per user/org and fetch a specific version's immutable snapshot.
type Management struct {
	versions *ScopeTable
	prices   *PriceCache

	mu      sync.Mutex
	now     func() time.Time
	horizon time.Duration
}

// NewManagement builds a management reader over the given version table and
// price cache (either may be nil, in which case that view is unavailable).
// The staleness horizon defaults to DailyInterval.
func NewManagement(versions *ScopeTable, prices *PriceCache) *Management {
	return &Management{
		versions: versions,
		prices:   prices,
		now:      time.Now,
		horizon:  DailyInterval,
	}
}

// SetNow injects the clock. Intended for tests only.
func (m *Management) SetNow(now func() time.Time) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if now != nil {
		m.now = now
	}
}

// SetHorizon overrides the staleness horizon used by PriceStatus.
func (m *Management) SetHorizon(h time.Duration) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if h > 0 {
		m.horizon = h
	}
}

// EffectiveView is the resolved in-force strategy for a scope.
type EffectiveView struct {
	// Strategy is the in-force strategy.
	Strategy Strategy
	// Found is true when a configured version was in force; false when the
	// built-in default applied.
	Found bool
	// Reason is the non-empty decision text explaining the choice.
	Reason string
}

// ListVersions returns the scope's full version history, oldest first. An
// empty result means the scope has no configured version, so the built-in
// default strategy applies to it.
func (m *Management) ListVersions(scope Scope) []Version {
	if m == nil || m.versions == nil {
		return nil
	}
	return m.versions.List(scope)
}

// GetVersion returns one version's immutable snapshot by its record number.
// ok=false means the number was never recorded for the scope, or it was
// already evicted FIFO.
func (m *Management) GetVersion(scope Scope, version int64) (Version, bool) {
	if m == nil || m.versions == nil {
		return Version{}, false
	}
	return m.versions.Get(scope, version)
}

// Effective reports the strategy in force for a scope: the newest configured
// version when one exists, else the built-in default with a non-empty reason.
func (m *Management) Effective(scope Scope) EffectiveView {
	if m == nil || m.versions == nil {
		return EffectiveView{Strategy: DefaultStrategy(), Reason: DefaultReason()}
	}
	s, found, reason := m.versions.Effective(scope)
	return EffectiveView{Strategy: s, Found: found, Reason: reason}
}

// PriceStatusView is a point-in-time description of the price cache.
type PriceStatusView struct {
	// Models is the number of priced models in the current snapshot.
	Models int
	// SyncedAt is when the current snapshot was taken; zero means never.
	SyncedAt time.Time
	// Stale is true when the snapshot is older than the staleness horizon.
	Stale bool
}

// PriceStatus returns a point-in-time view of the price cache. With no price
// cache wired, it reports an unavailable (stale, zero-model) view.
func (m *Management) PriceStatus() PriceStatusView {
	if m == nil || m.prices == nil {
		return PriceStatusView{Stale: true}
	}
	m.mu.Lock()
	now := m.now
	horizon := m.horizon
	m.mu.Unlock()
	prices, syncedAt := m.prices.Get()
	return PriceStatusView{
		Models:   len(prices),
		SyncedAt: syncedAt,
		Stale:    m.prices.IsStale(now(), horizon),
	}
}
