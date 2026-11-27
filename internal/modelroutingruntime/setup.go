// Package modelroutingruntime wires the internal/modelrouting request-level
// routing strategy engine (RIC-2522 schema + immutable versions + price cache,
// RIC-2524 decision engine, RIC-2525 offline replay) into the gateway request
// path (RIC-3351).
//
// It is a pure adapter layer, mirroring internal/fallbackruntime:
//
//   - it reads the declarative model-routing config block off the gateway
//     config,
//   - builds the engine runtime (version table, price cache, management
//     facade and the daily KEEPER price sync),
//   - installs the routing gate on the BaseAPIHandler through the
//     pre-reserved api.WithRouterConfigurator hook (the same extension point
//     the fallback bridge, the arbitrage model router and the route-boundary
//     gate use), and
//   - injects the GET/POST /v0/management/routing/policies handlers so the
//     strategy objects are manageable behind the standard management-key auth.
//
// A disabled or empty block builds nothing, so the default build keeps
// exactly the gateway's existing model resolution: no gate, no price-sync
// goroutine, no new route.
package modelroutingruntime

import (
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/api"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/modelrouting"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
)

var logger = logrus.WithField("component", "modelroutingruntime")

const (
	// EnvKeeperURL is the fallback KEEPER base URL for the daily price sync,
	// used when the config block leaves keeper-url empty.
	EnvKeeperURL = "CPA_MODEL_ROUTING_KEEPER_URL"
	// EnvKeeperKey is the shared X-CPA-Management-Key for the price pull.
	EnvKeeperKey = "CPA_MODEL_ROUTING_KEEPER_KEY"
	// EnvManagementKey is the management key reused from the content-filter /
	// arbitrage channels; it is the same shared secret.
	EnvManagementKey = "CPA_CONTENT_FILTER_AUDIT_KEEPER_KEY"
)

// Runtime is the live model-routing channel: the engine runtime plus the
// configured default strategy the gate applies to scopes without a version.
type Runtime struct {
	// core is the already-delivered modelrouting runtime (versions, prices,
	// management facade, price syncer). Never nil for a non-nil Runtime.
	core *modelrouting.Runtime
	// defaultStrategy is the configured default strategy, or nil to keep the
	// engine's built-in default (balanced, no floor, no cap).
	defaultStrategy *modelrouting.Strategy
	// statePath is the durable store for the version history. Empty keeps the
	// history in memory only (policies then do not survive a restart).
	statePath string
	// stateMu guards scopes and serializes writes to statePath.
	stateMu sync.Mutex
	// scopes indexes every scope holding a history, because the engine's
	// table only answers per-scope lookups.
	scopes map[modelrouting.Scope]struct{}
	// loadFailed is set when the state file existed but could not be read
	// back. Writes then go to a sibling ".recovered-*" file instead of the
	// original, so one damaged byte cannot cost an operator every other
	// scope's policy on the next POST.
	loadFailed bool
	// totalScopes / restoredScopes describe a partial load, so the failure log
	// can tell the operator what survived ("3 of 9 scopes") instead of
	// implying everything did.
	totalScopes    int
	restoredScopes int

	// startWG tracks the off-path start, so Stop can never race a syncer
	// that has not registered its cancel func yet. lifecycleMu guards the
	// start/stop handshake (startWG's Add must not race Wait).
	startWG     sync.WaitGroup
	lifecycleMu sync.Mutex
	stopped     bool
	// warned* give each deliberate pass-through state one warning line
	// instead of one per request.
	warnedNoPrices    atomic.Bool
	warnedNoCandidate atomic.Bool
	warnedUnmetFloor  atomic.Bool

	// servableMu/servable* memoise the servable-model set: the filter runs
	// one registry lock per priced model, which must not happen per request.
	servableMu         sync.RWMutex
	servable           map[string]modelrouting.Price
	servableGeneration uint64
	servableSyncedAt   time.Time
}

// Build constructs the model-routing channel from the gateway config. It
// returns nil when the block is missing, disabled, or carries an invalid
// default strategy: the channel is off and the default build's routing is
// untouched. An invalid strategy is logged rather than silently applied,
// matching the fallback bridge's "malformed config disables the channel"
// behavior.
func Build(cfg *config.Config) *Runtime {
	if cfg == nil || !cfg.ModelRouting.Enabled {
		return nil
	}
	block := cfg.ModelRouting

	var defaultStrategy *modelrouting.Strategy
	if block.DefaultStrategy != nil {
		strategy := strategyFromConfig(*block.DefaultStrategy)
		if err := modelrouting.Validate(strategy); err != nil {
			logger.WithError(err).Error("modelrouting: invalid model-routing.default-strategy; channel disabled")
			return nil
		}
		defaultStrategy = &strategy
	}

	interval := modelrouting.DailyInterval
	if raw := strings.TrimSpace(block.SyncInterval); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed <= 0 {
			logger.WithError(err).Errorf("modelrouting: invalid model-routing.sync-interval %q; channel disabled", raw)
			return nil
		}
		interval = parsed
	}

	core := modelrouting.Build(modelrouting.BuildOptions{
		KeeperURL:     keeperURLFromConfig(block.KeeperURL),
		ManagementKey: managementKeyFromConfig(block.ManagementKey),
		Interval:      interval,
	})
	runtime := &Runtime{
		core:            core,
		defaultStrategy: defaultStrategy,
		statePath:       strings.TrimSpace(block.StatePath),
		scopes:          make(map[modelrouting.Scope]struct{}),
	}
	if runtime.statePath != "" {
		if err := runtime.loadState(runtime.statePath); err != nil {
			// A corrupt management-state file must not take the data plane
			// down: log loudly and keep serving with an empty history. It must
			// also not be overwritten later (see persist), or the first policy
			// edit would erase every other scope's history.
			runtime.loadFailed = true
			logger.WithError(err).Errorf(
				"modelrouting: could not load the routing state file (%d of %d scope(s) restored); "+
					"edits will be written to a .recovered file until it is reconciled",
				runtime.restoredScopes, runtime.totalScopes)
		}
	}
	return runtime
}

// Start kicks off the daily price sync. A nil runtime (channel disabled) is a
// no-op, so the default build starts no goroutine.
//
// The engine's Start performs one synchronous initial pull (bounded by the
// 10s pull timeout). It runs off the boot path: a black-holed KEEPER URL must
// not delay the gateway accepting requests. Until the first snapshot lands the
// gate passes requests through, so nothing is lost by starting late.
func (r *Runtime) Start() {
	if r == nil || r.core == nil {
		return
	}
	r.lifecycleMu.Lock()
	if r.stopped {
		// Stop already ran (possible only in a narrow shutdown-before-start
		// window): don't spawn a syncer nobody will ever stop.
		r.lifecycleMu.Unlock()
		return
	}
	r.startWG.Add(1)
	r.lifecycleMu.Unlock()
	go func() {
		defer r.startWG.Done()
		r.core.Start()
	}()
}

// Stop drains the price sync loop. It waits for the off-path start to finish
// first, so it can never race a syncer that has not registered its cancel func
// yet — which would leak the goroutine. Setting stopped before the wait also
// closes the shutdown-before-start window: a Start that has not yet run its
// Add(1) sees the flag and does nothing.
func (r *Runtime) Stop() {
	if r == nil || r.core == nil {
		return
	}
	r.lifecycleMu.Lock()
	r.stopped = true
	r.lifecycleMu.Unlock()
	r.startWG.Wait()
	r.core.Stop()
}

// ServerOption installs the routing gate on the constructed server through the
// pre-reserved WithRouterConfigurator hook. It returns nil for a nil runtime so
// callers can skip it and keep the default request path.
func ServerOption(runtime *Runtime) api.ServerOption {
	if runtime == nil || runtime.core == nil {
		return nil
	}
	return api.WithRouterConfigurator(func(_ *gin.Engine, baseHandler *handlers.BaseAPIHandler, _ *config.Config) {
		if baseHandler == nil {
			return
		}
		baseHandler.SetModelRoutingGate(runtime)
		logger.Info("modelrouting: routing gate installed (request-level strategy engine)")
	})
}

// HandlersServerOption injects the strategy-object management handlers. They
// are registered inside the authenticated /v0/management group, never on the
// bare engine. It returns nil for a nil runtime.
func HandlersServerOption(runtime *Runtime) api.ServerOption {
	if runtime == nil || runtime.core == nil {
		return nil
	}
	return api.WithModelRoutingHandlers(runtime.GetPolicies, runtime.PostPolicies)
}

// strategyFromConfig converts the declarative config form into the frozen
// modelrouting.Strategy contract.
func strategyFromConfig(cfg config.ModelRoutingStrategyConfig) modelrouting.Strategy {
	return modelrouting.Strategy{
		Optimization:   strings.TrimSpace(cfg.Optimization),
		QualityFloor:   cfg.QualityFloor,
		MaxBudget:      cfg.MaxBudget,
		PreferredModel: strings.TrimSpace(cfg.PreferredModel),
		OnlyAllowModel: strings.TrimSpace(cfg.OnlyAllowModel),
	}
}

// keeperURLFromConfig resolves the price source URL, falling back to the
// environment so a deployment can keep endpoints out of the config file.
func keeperURLFromConfig(configured string) string {
	if url := strings.TrimSpace(configured); url != "" {
		return url
	}
	return strings.TrimSpace(os.Getenv(EnvKeeperURL))
}

// managementKeyFromConfig resolves the shared management key, preferring the
// environment over the config file.
func managementKeyFromConfig(configured string) string {
	if key := strings.TrimSpace(os.Getenv(EnvKeeperKey)); key != "" {
		return key
	}
	if key := strings.TrimSpace(configured); key != "" {
		return key
	}
	return strings.TrimSpace(os.Getenv(EnvManagementKey))
}
