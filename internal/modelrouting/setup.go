package modelrouting

import (
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// logger is the package-level logger, following the component-scoped
// convention of internal/arbitrage and internal/policies.
var logger = logrus.WithField("component", "modelrouting")

// BuildOptions carries the dependencies for a running modelrouting runtime.
type BuildOptions struct {
	// KeeperURL is the KEEPER control-plane base URL for the daily price
	// sync (e.g. http://keeper:8080/keeper). When empty, the price cache
	// stays empty and routing falls back to the built-in default strategy —
	// the version table and default strategy remain fully available, so the
	// decision path never silently loses its output.
	KeeperURL string
	// ManagementKey is the shared KEEPER management key (the
	// X-CPA-Management-Key channel, as used by internal/arbitrage and
	// internal/policies).
	ManagementKey string
	// Interval is the price sync period; DailyInterval (24h) when unset.
	Interval time.Duration
	// StalenessHorizon bounds how old a synced snapshot may be before
	// readers mark it stale; DailyInterval when unset.
	StalenessHorizon time.Duration
	// Now injects the clock for deterministic tests.
	Now func() time.Time
}

// Runtime bundles the live components so the caller can drive and stop them,
// mirroring internal/arbitrage.Runtime.
type Runtime struct {
	// Versions is the immutable strategy version history.
	Versions *ScopeTable
	// Prices is the daily-synced price cache.
	Prices *PriceCache
	// Management is the read-only management API facade over both.
	Management *Management
	// Syncer drives the KEEPER price pull; nil when no KEEPER source is
	// configured, in which case Start/Stop are no-ops on the price path.
	Syncer *Syncer
	// StalenessHorizon is the horizon readers use for the stale flag.
	StalenessHorizon time.Duration
}

// Build constructs the modelrouting components. It always returns a usable
// runtime: the version table, default strategy and price cache exist even
// with no KEEPER source, so the Stage 2 decision engine always has a
// resolvable strategy ("routing always has an output").
func Build(opts BuildOptions) *Runtime {
	opts.withDefaults()
	table := NewScopeTable()
	table.SetNow(opts.Now)
	cache := NewPriceCache()
	management := NewManagement(table, cache)
	management.SetNow(opts.Now)
	management.SetHorizon(opts.StalenessHorizon)
	runtime := &Runtime{
		Versions:         table,
		Prices:           cache,
		Management:       management,
		StalenessHorizon: opts.StalenessHorizon,
	}

	if keeperURL := strings.TrimSpace(opts.KeeperURL); keeperURL != "" {
		source := &PriceSource{KeeperURL: keeperURL, ManagementKey: opts.ManagementKey}
		runtime.Syncer = NewSyncer(cache, source, SyncerOptions{
			Interval:         opts.Interval,
			StalenessHorizon: opts.StalenessHorizon,
			Now:              opts.Now,
		})
	}
	return runtime
}

func (o *BuildOptions) withDefaults() {
	if o.Interval <= 0 {
		o.Interval = DailyInterval
	}
	if o.StalenessHorizon <= 0 {
		o.StalenessHorizon = DailyInterval
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

// Start kicks off the background price sync, performing one synchronous
// initial pull so the cache converges immediately after boot. A failed
// initial pull keeps the cache empty (default strategy applies) and the
// background loop retries on the daily tick.
func (r *Runtime) Start() {
	if r == nil || r.Syncer == nil {
		return
	}
	r.Syncer.Start()
	logger.Info("modelrouting: daily KEEPER price sync started")
}

// Stop drains the sync loop.
func (r *Runtime) Stop() {
	if r == nil || r.Syncer == nil {
		return
	}
	r.Syncer.Stop()
}
