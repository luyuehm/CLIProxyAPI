package modelrouting

import (
	"testing"
	"time"
)

// TestDefaultStrategy covers acceptance scenario 4: a scope with no
// configured strategy still resolves to a built-in default, with a
// non-empty default model and a non-empty default reason.
func TestDefaultStrategy(t *testing.T) {
	scope := Scope{Kind: ScopeUser, ID: "new-user"}
	table := NewScopeTable()

	strategy, found, reason := table.Effective(scope)
	if found {
		t.Fatalf("an unconfigured scope must resolve to the default, not a stored version")
	}
	if strategy.PreferredModel == "" {
		t.Errorf("default strategy must name a default model, got %+v", strategy)
	}
	if strategy != DefaultStrategy() {
		t.Errorf("default mismatch: got %+v, want %+v", strategy, DefaultStrategy())
	}
	if reason == "" {
		t.Error("default reason text must be non-empty")
	}
	if DefaultReason() == "" || DefaultModel == "" {
		t.Error("DefaultReason/DefaultModel must be non-empty")
	}
}

// TestDefaultStrategyViaManagement covers the same guarantee through the
// management read API facade.
func TestDefaultStrategyViaManagement(t *testing.T) {
	table := NewScopeTable()
	mgmt := NewManagement(table, nil)

	view := mgmt.Effective(Scope{Kind: ScopeOrg, ID: "acme"})
	if view.Found {
		t.Fatal("an unconfigured scope must report the built-in default")
	}
	if view.Strategy == (Strategy{}) || view.Reason == "" {
		t.Fatalf("default view must carry a strategy and a reason, got %+v", view)
	}

	// After a version is recorded, the management view reflects it instead.
	if _, err := table.Update(Scope{Kind: ScopeOrg, ID: "acme"}, Record{
		Strategy: Strategy{Optimization: OptMinCost},
		Note:     "configured",
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	view = mgmt.Effective(Scope{Kind: ScopeOrg, ID: "acme"})
	if !view.Found || view.Strategy.Optimization != OptMinCost {
		t.Fatalf("configured scope must report its version, got %+v", view)
	}
}

// TestDefaultPriceStatus verifies the management price view reports a
// never-synced cache as stale, and reflects a synced one.
func TestDefaultPriceStatus(t *testing.T) {
	mgmt := NewManagement(nil, NewPriceCache())
	status := mgmt.PriceStatus()
	if !status.Stale {
		t.Fatal("a never-synced price cache must report stale")
	}
	if status.Models != 0 {
		t.Fatalf("never-synced cache must hold no models, got %d", status.Models)
	}
}

// TestDefaultBuild verifies the Build entry point always returns a usable
// runtime: without a KEEPER source the version table, default strategy and
// price cache still exist, so routing never silently loses its output.
func TestDefaultBuild(t *testing.T) {
	runtime := Build(BuildOptions{Now: func() time.Time { return time.Now() }})
	if runtime == nil {
		t.Fatal("Build must always return a runtime")
	}
	if runtime.Versions == nil || runtime.Prices == nil || runtime.Management == nil {
		t.Fatalf("runtime must expose the version table, price cache and management facade, got %+v", runtime)
	}
	if runtime.Syncer != nil {
		t.Fatal("without a KEEPER source the price syncer must stay nil (channel off)")
	}
	// The unconfigured scope still resolves to the built-in default.
	view := runtime.Management.Effective(Scope{Kind: ScopeUser, ID: "nobody"})
	if view.Found || view.Reason == "" {
		t.Fatalf("unconfigured scope must resolve to the default with a reason, got %+v", view)
	}
}

// TestBuildWithKeeper wires the KEEPER-backed price syncer and proves the
// daily sync drives the cache, with the management facade reflecting it.
func TestBuildWithKeeper(t *testing.T) {
	runtime := Build(BuildOptions{
		KeeperURL: "http://keeper:8080", // no live pull: Start is not called here
		Now:       func() time.Time { return time.Now() },
	})
	if runtime.Syncer == nil {
		t.Fatal("a KEEPER source must wire the price syncer")
	}
	if !runtime.Management.PriceStatus().Stale {
		t.Fatal("before any sync the price view must report stale")
	}
}
