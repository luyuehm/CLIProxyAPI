// Package fallbackruntime wires the internal/fallback provider-fallback
// engine (RIC-3143, OPENROUT-001) into the gateway request path.
//
// It is a pure adapter layer: it reads the declarative fallback config block
// off the gateway config, builds the engine bridge, and installs it on the
// BaseAPIHandler through the pre-reserved api.WithRouterConfigurator hook
// (the same extension point the model router host and the route-boundary gate
// use). A disabled or empty fallback block installs nothing, so the default
// build keeps exactly the gateway's existing credential-level failover.
package fallbackruntime

import (
	"time"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/api"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/fallback"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/providerlatency"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
)

var logger = logrus.WithField("component", "fallbackruntime")

// ServerOption installs the automatic provider fallback bridge on the
// constructed server through the pre-reserved WithRouterConfigurator hook.
// It returns nil when the config disables the engine or the chain is empty,
// so the default build routes exactly as before.
func ServerOption(cfg *config.Config) api.ServerOption {
	if cfg == nil || !cfg.Fallback.Enabled || len(cfg.Fallback.Providers) == 0 {
		return nil
	}
	fallbackCfg := configToFallbackConfig(cfg.Fallback)
	return api.WithRouterConfigurator(func(_ *gin.Engine, baseHandler *handlers.BaseAPIHandler, _ *config.Config) {
		if baseHandler == nil {
			return
		}
		installed, err := baseHandler.SetFallbackEngine(fallbackCfg, true)
		if err != nil {
			logger.WithError(err).Error("fallback: failed to build engine; provider fallback disabled")
			return
		}
		if !installed {
			return
		}
		logger.Info("fallback: provider fallback engine installed (auto-degrade on provider failure)")
	})
}

// configToFallbackConfig converts the gateway config block into the
// declarative fallback.Config the engine consumes.
func configToFallbackConfig(cfg config.FallbackConfig) fallback.Config {
	out := fallback.Config{
		CostLimitUSD:  cfg.CostLimitUSD,
		QualityFloor:  cfg.QualityFloor,
		TimeoutBudget: cfg.TimeoutBudget,
		BackoffWindow: cfg.BackoffWindow,
		BackoffBase:   cfg.BackoffBase,
		BackoffMax:    cfg.BackoffMax,
	}
	for _, ref := range cfg.Providers {
		out.Providers = append(out.Providers, fallback.ProviderRef{
			Provider: ref.Provider,
			Model:    ref.Model,
			Quality:  ref.Quality,
			CostUSD:  ref.CostUSD,
		})
	}
	return out
}

// ProviderPrefsOption installs the in-process provider-latency window
// (RIC-3352 option A, docs/provider-prefs-decision.md) on the constructed
// server through the same pre-reserved WithRouterConfigurator hook as the
// fallback engine. It returns nil when provider-prefs is not enabled, so the
// default build measures nothing: a request-level latency sort degrades to
// the existing static order and is recorded as latency_source_empty in
// routing_trace.
//
// The window is shared: the handler reads it for sort ranking
// (SetProviderPrefsLatency) and the fallback bridge records each completed
// attempt into it, so ranking is on measured, per-instance data — never a
// live probe, so routing can never block on it (the frozen RIC-3307
// disposition).
func ProviderPrefsOption(cfg *config.Config) api.ServerOption {
	if cfg == nil || !cfg.ProviderPrefs.Enabled {
		return nil
	}
	window := buildProviderPrefsWindow(cfg.ProviderPrefs)
	return api.WithRouterConfigurator(func(_ *gin.Engine, baseHandler *handlers.BaseAPIHandler, _ *config.Config) {
		if baseHandler == nil {
			return
		}
		baseHandler.SetProviderPrefsLatency(window)
		if baseHandler.FallbackBridge != nil {
			// The bridge is installed by ServerOption, which runs before this
			// hook (builder order), so the recorder catches every fallback
			// attempt's wall latency.
			baseHandler.FallbackBridge.SetLatencyRecorder(func(provider string, totalMS int64) {
				window.Record(provider, totalMS, time.Now())
			})
		}
		logger.Info("provider-prefs: in-process latency window installed (sort:latency ranks on measured attempt latency)")
	})
}

// buildProviderPrefsWindow builds the latency window from the config block,
// defaulting the window horizon. The horizon is best-effort (an unparseable
// duration keeps the default), matching the fallback backoff-duration rule:
// a misconfigured value must not drop the whole source.
func buildProviderPrefsWindow(cfg config.ProviderPrefsConfig) *providerlatency.Window {
	opts := providerlatency.Options{}
	if w, err := time.ParseDuration(cfg.Window); err == nil && w > 0 {
		opts.Window = w
	}
	return providerlatency.NewWindow(opts)
}
