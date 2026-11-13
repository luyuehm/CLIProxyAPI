package api

import (
	"context"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/pluginhost"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type serverOptionConfig struct {
	extraMiddleware       []gin.HandlerFunc
	engineConfigurator    func(*gin.Engine)
	routerConfigurators   []func(*gin.Engine, *handlers.BaseAPIHandler, *config.Config)
	requestLoggerFactory  func(*config.Config, string) logging.RequestLogger
	localPassword         string
	keepAliveEnabled      bool
	keepAliveTimeout      time.Duration
	keepAliveOnTimeout    func()
	postAuthHook          auth.PostAuthHook
	postAuthPersistHook   auth.PostAuthHook
	pluginHost            *pluginhost.Host
	configReloadHook      func(context.Context, *config.Config)
	exampleAPIKeySafeMode bool
	// contentFilterExportHandler is the RIC-443 audit export endpoint. It is
	// injected by the contentfilter package (which cannot import this one —
	// import cycle) and registered under /v0/management/contentfilter/export
	// so it rides the same management-key auth as the other management routes.
	contentFilterExportHandler gin.HandlerFunc
	// arbitrageStatusHandler is the RIC-2458 arbitration status endpoint. It is
	// injected by internal/arbitrage (which imports this package) and registered
	// inside the authenticated /v0/management group so it inherits the same
	// management-key auth as every other management route.
	arbitrageStatusHandler gin.HandlerFunc
}

// ServerOption customises HTTP server construction.
type ServerOption func(*serverOptionConfig)

func defaultRequestLoggerFactory(cfg *config.Config, configPath string) logging.RequestLogger {
	configDir := filepath.Dir(configPath)
	logsDir := logging.ResolveLogDirectory(cfg)
	logger := logging.NewFileRequestLogger(cfg.RequestLog, logsDir, configDir, cfg.ErrorLogsMaxFiles)
	logger.SetHomeEnabled(cfg != nil && cfg.Home.Enabled)
	return logger
}

func effectiveSDKConfig(cfg *config.Config) *config.SDKConfig {
	if cfg == nil {
		return nil
	}
	sdkCfg := cfg.SDKConfig
	sdkCfg.CodexOptimizeMultiAgentV2 = cfg.Codex.OptimizeMultiAgentV2
	sdkCfg.CodexOrphanDelegationCompatibility = cfg.Codex.OrphanDelegationCompatibility
	if cfg.CommercialMode {
		sdkCfg.RequestLog = false
	}
	return &sdkCfg
}

// WithMiddleware appends additional Gin middleware during server construction.
func WithMiddleware(mw ...gin.HandlerFunc) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.extraMiddleware = append(cfg.extraMiddleware, mw...)
	}
}

// WithContentFilterExportHandler injects the RIC-443 audit export endpoint.
// The handler lives in internal/contentfilter (which imports this package, so
// the route handler cannot be constructed here); the contentfilter package
// passes its handler through this option and the server registers it under
// /v0/management/contentfilter/export.
func WithContentFilterExportHandler(h gin.HandlerFunc) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.contentFilterExportHandler = h
	}
}

// WithArbitrageStatusHandler injects the RIC-2458 arbitration status endpoint.
// It is registered inside the authenticated /v0/management group; registering it
// on the bare engine would expose routing topology and internal endpoints to any
// caller that can reach the port.
func WithArbitrageStatusHandler(h gin.HandlerFunc) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.arbitrageStatusHandler = h
	}
}

// WithEngineConfigurator allows callers to mutate the Gin engine prior to middleware setup.
func WithEngineConfigurator(fn func(*gin.Engine)) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.engineConfigurator = fn
	}
}

// WithRouterConfigurator appends a callback after default routes are
// registered. Multiple configurators run in the order they were appended, so
// independent channels (the arbitrage model router host, the KEEPER route
// boundary gate) can each install their hooks on the same handler.
func WithRouterConfigurator(fn func(*gin.Engine, *handlers.BaseAPIHandler, *config.Config)) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.routerConfigurators = append(cfg.routerConfigurators, fn)
	}
}

// WithLocalManagementPassword stores a runtime-only management password accepted for localhost requests.
func WithLocalManagementPassword(password string) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.localPassword = password
	}
}

// WithKeepAliveEndpoint enables a keep-alive endpoint with the provided timeout and callback.
func WithKeepAliveEndpoint(timeout time.Duration, onTimeout func()) ServerOption {
	return func(cfg *serverOptionConfig) {
		if timeout <= 0 || onTimeout == nil {
			return
		}
		cfg.keepAliveEnabled = true
		cfg.keepAliveTimeout = timeout
		cfg.keepAliveOnTimeout = onTimeout
	}
}

// WithRequestLoggerFactory customises request logger creation.
func WithRequestLoggerFactory(factory func(*config.Config, string) logging.RequestLogger) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.requestLoggerFactory = factory
	}
}

// WithPostAuthHook registers a hook to be called after auth record creation.
func WithPostAuthHook(hook auth.PostAuthHook) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.postAuthHook = hook
	}
}

// WithPostAuthPersistHook registers a hook to be called after auth persistence.
func WithPostAuthPersistHook(hook auth.PostAuthHook) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.postAuthPersistHook = hook
	}
}

// WithPluginHost registers dynamic plugin HTTP adapters with the server.
func WithPluginHost(host *pluginhost.Host) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.pluginHost = host
	}
}

// WithConfigReloadHook registers a callback used after management saves config changes.
func WithConfigReloadHook(hook func(context.Context, *config.Config)) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.configReloadHook = hook
	}
}

// WithExampleAPIKeySafeMode blocks proxy API endpoints while template API keys remain configured.
func WithExampleAPIKeySafeMode() ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.exampleAPIKeySafeMode = true
	}
}
