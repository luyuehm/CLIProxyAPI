package routeboundary

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/api"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// Environment variables controlling the KEEPER route-boundary channel. All
// are optional; the feature is off by default so the gateway routes exactly
// as an unconfigured build when nothing is set.
const (
	// EnvEnabled forces the boundary channel on/off. "1"|"true"|"yes"|"on"
	// enables, "0"|"false"|"no"|"off" disables. Unset means disabled.
	EnvEnabled = "CPA_ROUTE_BOUNDARY_ENABLED"
	// EnvKeeperURL is the KEEPER control-plane base URL (e.g.
	// http://127.0.0.1:4320, or http://keeper:8080/keeper when the
	// deployment sets APP_BASE_PATH=/keeper). When set alongside
	// EnvKeeperKey the puller is active. If empty, it is derived from
	// CPA_BASE_URL (port → 4320), matching the sibling channels.
	EnvKeeperURL = "CPA_ROUTE_BOUNDARY_KEEPER_URL"
	// EnvKeeperKey is the shared X-CPA-Management-Key used to authenticate
	// the CPA-to-KEEPER boundary pull and audit ingest.
	EnvKeeperKey = "CPA_ROUTE_BOUNDARY_KEEPER_KEY"
	// EnvIntervalSec overrides the boundary pull interval in seconds
	// (default 5).
	EnvIntervalSec = "CPA_ROUTE_BOUNDARY_INTERVAL_SECONDS"
	// EnvManagementKey is reused from the content-filter/policy channels;
	// it is the same shared key as EnvKeeperKey. Setting either enables
	// the channel.
	EnvManagementKey = "CPA_CONTENT_FILTER_AUDIT_KEEPER_KEY"
)

// Runtime bundles the live components so the caller can drive and stop them
// (the arbitrage.Build pattern): the snapshot store, the KEEPER poller, the
// credential-availability probe, the execution gate, the admission
// middleware, and the override audit writer.
type Runtime struct {
	Store      *Store
	Puller     *Puller
	Enforcer   *Enforcer
	Middleware *Middleware
	Audit      *Audit
}

// Build constructs the route-boundary runtime components for a running
// service. It returns nil when the channel is disabled or under-configured,
// so the caller installs nothing and the gateway keeps its default routing
// — the safe default for a deployment that has not configured KEEPER.
//
// manager supplies the live credential set for the strict/BYOK
// availability decisions; a nil manager degrades those decisions to pure
// boundary narrowing (fail-open).
func Build(manager *coreauth.Manager) *Runtime {
	if !enabledFromEnv() {
		logger.Debug("KEEPER route-boundary channel disabled (set " + EnvEnabled + "=true to enable)")
		return nil
	}
	keeperURL, keeperKey := keeperFromEnv()
	if keeperURL == "" || keeperKey == "" {
		logger.Warn("KEEPER route-boundary channel disabled (KEEPER URL and management key are both required)")
		return nil
	}

	store := NewStore()
	puller := NewPuller(store, PullerOptions{
		KeeperURL:     keeperURL,
		ManagementKey: keeperKey,
		Interval:      intervalFromEnv(),
	})
	audit := NewAudit(AuditOptions{
		KeeperURL:     keeperURL,
		ManagementKey: keeperKey,
	})
	probe := NewManagerProbe(manager)
	enforcer := NewEnforcer(EnforcerOptions{Store: store, Probe: probe})
	mw := NewMiddleware(MiddlewareOptions{Store: store, Audit: audit})

	return &Runtime{
		Store:      store,
		Puller:     puller,
		Enforcer:   enforcer,
		Middleware: mw,
		Audit:      audit,
	}
}

// Start kicks off the boundary poll loop and performs the first pull
// synchronously. It is idempotent and a no-op for a nil runtime.
func (r *Runtime) Start() {
	if r == nil {
		return
	}
	if r.Puller != nil {
		r.Puller.Start()
	}
}

// Stop drains the poll loop and the audit writer.
func (r *Runtime) Stop() {
	if r == nil {
		return
	}
	if r.Puller != nil {
		r.Puller.Stop()
	}
	if r.Audit != nil {
		r.Audit.Close()
	}
}

// MiddlewareServerOption installs the admission middleware through the
// pre-reserved api.WithMiddleware() extension point. It returns nil for a
// nil runtime so callers can skip it and keep default behaviour.
func (r *Runtime) MiddlewareServerOption() api.ServerOption {
	if r == nil || r.Middleware == nil {
		return nil
	}
	return api.WithMiddleware(r.Middleware.Handler())
}

// ExecutionServerOption installs the execution provider-pool gate on the
// constructed server through the pre-reserved WithRouterConfigurator hook
// (the SetModelRouterHost precedent), which avoids touching any upstream
// handler or route file. It returns nil for a nil runtime.
func (r *Runtime) ExecutionServerOption() api.ServerOption {
	if r == nil || r.Enforcer == nil {
		return nil
	}
	gate := r.Enforcer
	return api.WithRouterConfigurator(func(_ *gin.Engine, baseHandler *handlers.BaseAPIHandler, _ *config.Config) {
		if baseHandler != nil {
			baseHandler.SetRouteBoundaryGate(gate)
			logger.Info("routeboundary: execution provider-pool gate installed (fallback cannot leave the key's boundary)")
		}
	})
}

// enabledFromEnv resolves the enable switch.
func enabledFromEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvEnabled))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return false
	}
}

// keeperFromEnv resolves the KEEPER control-plane connection.
func keeperFromEnv() (url, key string) {
	url = strings.TrimSpace(os.Getenv(EnvKeeperURL))
	if url == "" {
		// Dev fallback: derive KEEPER's 4320 port from CPA_BASE_URL,
		// matching the content-filter and policy channels.
		if base := strings.TrimSpace(os.Getenv("CPA_BASE_URL")); base != "" {
			url = deriveKeeperURL(base)
		}
	}
	key = strings.TrimSpace(os.Getenv(EnvKeeperKey))
	if key == "" {
		key = strings.TrimSpace(os.Getenv(EnvManagementKey))
	}
	return url, key
}

// intervalFromEnv reads the pull interval override in seconds.
func intervalFromEnv() time.Duration {
	seconds := envPositiveInt64(os.Getenv(EnvIntervalSec), int64(DefaultInterval/time.Second))
	return time.Duration(seconds) * time.Second
}

func envPositiveInt64(value string, fallback int64) int64 {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil || parsed < 0 {
		return fallback
	}
	return parsed
}

// deriveKeeperURL replaces the port of a CPA base URL with 4320 (KEEPER's
// default). Only a dev/local fallback; production should configure
// EnvKeeperURL explicitly.
func deriveKeeperURL(base string) string {
	u := strings.TrimRight(base, "/")
	idx := strings.LastIndex(u, ":")
	if idx < 0 {
		return u + ":4320"
	}
	return u[:idx] + ":4320"
}
