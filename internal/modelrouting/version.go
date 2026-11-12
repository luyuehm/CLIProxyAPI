package modelrouting

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// maxVersions is the retention cap per scope: when exceeded, the oldest
// records are dropped FIFO. This period deliberately keeps no archive —
// a business need for longer retention is a blocked condition, not a
// silent extension.
const maxVersions = 500

// Version is one immutable record in a scope's strategy history. Once
// stored, a version is never mutated; a strategy change always appends a
// new record with the complete snapshot at that instant.
type Version struct {
	// Version is the scope-local monotonically increasing record number.
	Version int64 `json:"version"`
	// Timestamp is when the record was written. Records within one scope
	// are strictly increasing in time.
	Timestamp time.Time `json:"timestamp"`
	// Strategy is the full snapshot in force as of this record.
	Strategy Strategy `json:"strategy"`
	// Note is the operator-supplied change note (may be empty).
	Note string `json:"note,omitempty"`
}

// Record is a validated strategy plus its change note, ready to become the
// newest version of a scope.
type Record struct {
	Strategy Strategy
	Note     string
}

// ScopeTable holds every scope's version history. All records are
// immutable; Update appends a new record rather than rewriting history.
// The table is safe for concurrent use: readers take a read lock over one
// scope's slice and the writers swap slices under the write lock, so a
// reader never observes a half-written record.
type ScopeTable struct {
	mu      sync.RWMutex
	byScope map[string][]Version
	now     func() time.Time
}

// NewScopeTable returns an empty table. The clock is time.Now by default;
// tests inject a deterministic one so version timestamps are assertable.
func NewScopeTable() *ScopeTable {
	return &ScopeTable{
		byScope: make(map[string][]Version),
		now:     time.Now,
	}
}

// key is the table's storage key for a scope.
func key(s Scope) string {
	return s.Kind + ":" + s.ID
}

// SetNow injects the clock. Intended for tests only.
func (t *ScopeTable) SetNow(now func() time.Time) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if now != nil {
		t.now = now
	}
}

// Update validates rec and appends it as the scope's newest version. The
// record number is the scope's previous maximum plus one, and its
// timestamp is strictly after the previous record's: when the clock has
// not advanced between two writes the instant is nudged one nanosecond
// forward, so the immutability invariant holds even for fast consecutive
// updates.
//
// FIFO eviction: once a scope holds more than maxVersions records the
// oldest ones are dropped. Dropping is not version mutation — surviving
// records keep their numbers and content, only retention shrinks.
func (t *ScopeTable) Update(scope Scope, rec Record) (Version, error) {
	if scope.Kind == "" || scope.ID == "" {
		return Version{}, fmt.Errorf("modelrouting: scope requires kind and id")
	}
	if err := Validate(rec.Strategy); err != nil {
		return Version{}, err
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	entries := t.byScope[key(scope)]
	version := int64(1)
	timestamp := t.now()
	if len(entries) > 0 {
		last := entries[len(entries)-1]
		version = last.Version + 1
		if !timestamp.After(last.Timestamp) {
			// The clock did not advance between the two records (frozen
			// test clock or sub-nanosecond updates): nudge the instant
			// forward so the strictly-increasing invariant still holds.
			timestamp = last.Timestamp.Add(1 * time.Nanosecond)
		}
	}
	record := Version{
		Version:   version,
		Timestamp: timestamp,
		Strategy:  rec.Strategy,
		Note:      strings.TrimSpace(rec.Note),
	}
	entries = append(entries, record)
	if len(entries) > maxVersions {
		drop := len(entries) - maxVersions
		entries = append([]Version(nil), entries[drop:]...)
	}
	t.byScope[key(scope)] = entries
	return record, nil
}

// List returns the scope's version history, oldest first. A scope without
// any recorded version returns an empty list — the built-in default
// strategy then applies to that scope.
func (t *ScopeTable) List(scope Scope) []Version {
	if t == nil {
		return nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	entries := t.byScope[key(scope)]
	out := make([]Version, len(entries))
	copy(out, entries)
	return out
}

// Get returns one version by its record number. ok=false means the number
// was never recorded for the scope, or it was already evicted FIFO.
func (t *ScopeTable) Get(scope Scope, version int64) (Version, bool) {
	if t == nil {
		return Version{}, false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, entry := range t.byScope[key(scope)] {
		if entry.Version == version {
			return entry, true
		}
	}
	return Version{}, false
}

// Effective resolves the strategy in force for a scope: the newest
// recorded version when one exists, otherwise the built-in default. The
// (found, reason) pair is what the decision engine's "routing always has
// an output" guarantee consumes — found reports whether a configured
// version was in force, and reason is the non-empty text explaining the
// choice.
func (t *ScopeTable) Effective(scope Scope) (Strategy, bool, string) {
	if t == nil {
		return DefaultStrategy(), false, DefaultReason()
	}
	versions := t.List(scope)
	if len(versions) == 0 {
		return DefaultStrategy(), false, DefaultReason()
	}
	latest := versions[len(versions)-1]
	return latest.Strategy, true, fmt.Sprintf("strategy v%d in force", latest.Version)
}
