package modelrouting

import (
	"testing"
	"time"
)

func TestVersionImmutable(t *testing.T) {
	scope := Scope{Kind: ScopeUser, ID: "alice"}
	table := NewScopeTable()

	frozen := time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)
	table.SetNow(func() time.Time { return frozen })

	s1 := Strategy{Optimization: OptMinCost, QualityFloor: 0.5}
	v1, err := table.Update(scope, Record{Strategy: s1, Note: "initial"})
	if err != nil {
		t.Fatalf("first version: %v", err)
	}
	if v1.Version != 1 {
		t.Fatalf("first version = %d, want 1", v1.Version)
	}

	// A second, different strategy must not mutate v1's snapshot: the audit
	// guarantee that every past version is still retrievable verbatim.
	s2 := Strategy{Optimization: OptHighQuality, QualityFloor: 0.9}
	v2, err := table.Update(scope, Record{Strategy: s2, Note: "raised floor"})
	if err != nil {
		t.Fatalf("second version: %v", err)
	}

	got1, ok := table.Get(scope, 1)
	if !ok {
		t.Fatalf("v1 not retrievable after v2 was written")
	}
	got2, ok := table.Get(scope, 2)
	if !ok {
		t.Fatalf("v2 not retrievable")
	}
	if got1.Strategy != s1 {
		t.Errorf("v1 snapshot mutated: got %+v, want %+v", got1.Strategy, s1)
	}
	if got2.Strategy != s2 {
		t.Errorf("v2 snapshot wrong: got %+v, want %+v", got2.Strategy, s2)
	}
	// Timestamps must be strictly increasing even on a frozen clock.
	if !got2.Timestamp.After(got1.Timestamp) {
		t.Errorf("v2 timestamp %v must be after v1 %v", got2.Timestamp, got1.Timestamp)
	}
	if v1.Version != 1 || v2.Version != 2 {
		t.Errorf("version numbering = %d, %d; want 1, 2", v1.Version, v2.Version)
	}

	// The in-force strategy is the newest record.
	strategy, found, reason := table.Effective(scope)
	if !found || strategy != s2 {
		t.Errorf("Effective = (%+v, %v, %q); want v2 strategy found", strategy, found, reason)
	}
	if reason == "" {
		t.Error("Effective reason must be non-empty")
	}

	// An unknown scope has no versions.
	if versions := table.List(Scope{Kind: ScopeOrg, ID: "acme"}); len(versions) != 0 {
		t.Errorf("unknown scope should have no versions, got %d", len(versions))
	}
	if _, ok := table.Get(scope, 99); ok {
		t.Error("unknown version number must not resolve")
	}
}

// TestVersionFIFOEjection verifies the retention cap: oldest records are
// dropped FIFO, but surviving records keep their numbers and content.
func TestVersionFIFOEjection(t *testing.T) {
	scope := Scope{Kind: ScopeOrg, ID: "acme"}
	table := NewScopeTable()

	frozen := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	table.SetNow(func() time.Time { return frozen })

	s := Strategy{Optimization: OptBalanced}
	for i := 0; i < maxVersions+1; i++ {
		if _, err := table.Update(scope, Record{Strategy: s, Note: "tick"}); err != nil {
			t.Fatalf("update %d: %v", i, err)
		}
	}

	if versions := table.List(scope); len(versions) != maxVersions {
		t.Fatalf("retained %d versions, want the cap %d", len(versions), maxVersions)
	}
	// The oldest record (v1) was evicted; the newest (v+1) is present and
	// its number was not compacted.
	if _, ok := table.Get(scope, 1); ok {
		t.Error("oldest version should have been evicted FIFO")
	}
	if _, ok := table.Get(scope, int64(maxVersions+1)); !ok {
		t.Error("newest version missing after eviction")
	}
}

// TestVersionInvalidRejected verifies that an invalid strategy never becomes
// a version and does not disturb existing ones.
func TestVersionInvalidRejected(t *testing.T) {
	scope := Scope{Kind: ScopeUser, ID: "bob"}
	table := NewScopeTable()

	if _, err := table.Update(scope, Record{Strategy: Strategy{Optimization: "fast"}}); err == nil {
		t.Fatal("invalid strategy must be rejected")
	}
	if versions := table.List(scope); len(versions) != 0 {
		t.Fatalf("rejected update must not create a version, got %d", len(versions))
	}
	if _, err := table.Update(Scope{}, Record{Strategy: DefaultStrategy()}); err == nil {
		t.Fatal("scope without kind/id must be rejected")
	}
}
