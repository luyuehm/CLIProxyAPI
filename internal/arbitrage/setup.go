package arbitrage

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/api"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/sirupsen/logrus"
)

// Environment variables controlling the forwarding-side arbitration consumer.
// All are optional; the feature is off by default so the gateway behaves exactly
// as an unconfigured build when nothing is set.
const (
	// EnvEnabled forces the channel on/off. "1"|"true"|"yes"|"on" enables.
	// Unset means disabled.
	EnvEnabled = "CPA_ARBITRAGE_ENABLED"
	// EnvKeeperURL is the KEEPER control-plane base URL. When set alongside a
	// management key the puller is active; otherwise it is derived from
	// CPA_BASE_URL (port → 4320), matching the sibling channels.
	EnvKeeperURL = "CPA_ARBITRAGE_KEEPER_URL"
	// EnvKeeperKey is the shared X-CPA-Management-Key for the pull.
	EnvKeeperKey = "CPA_ARBITRAGE_KEEPER_KEY"
	// EnvIntervalSec overrides the decision pull interval in seconds.
	EnvIntervalSec = "CPA_ARBITRAGE_INTERVAL_SECONDS"
	// EnvStalenessFactor overrides how many KEEPER intervals a snapshot stays
	// usable. Larger values tolerate longer KEEPER outages at the cost of
	// acting on staler prices.
	EnvStalenessFactor = "CPA_ARBITRAGE_STALENESS_FACTOR"
	// EnvManagementKey is reused from the content-filter/policy channels; it is
	// the same shared key as EnvKeeperKey.
	EnvManagementKey = "CPA_CONTENT_FILTER_AUDIT_KEEPER_KEY"
	// EnvAllowInsecureEndpoints permits http:// and non-public endpoints. Only
	// for local development: with it set, a hostile decision feed can point the
	// gateway (and the operator's provider key) at internal addresses.
	EnvAllowInsecureEndpoints = "CPA_ARBITRAGE_ALLOW_INSECURE_ENDPOINTS"
	// EnvAllowedEndpointHosts is an optional comma-separated hostname allowlist
	// for delivered endpoints, on top of the address checks.
	EnvAllowedEndpointHosts = "CPA_ARBITRAGE_ALLOWED_ENDPOINT_HOSTS"
	// EnvAuthRefs maps management-plane credential reference names to local
	// credentials as "ref=apiKey" pairs, comma separated. The arbitration
	// contract never carries key material, so this is where the gateway learns
	// what ProviderPrice.AuthRef values resolve to on this node. Unset disables
	// the channel (no resolver → no routing change).
	EnvAuthRefs = "CPA_ARBITRAGE_AUTH_REFS"
)

// logger is the package-level logger.
var logger = logrus.WithField("component", "arbitrage")

// Runtime bundles the live components so the caller can drive and stop them.
type Runtime struct {
	Store      *Store
	Puller     *Puller
	Router     *RouterHost
	Reconciler *Reconciler
}

// Start kicks off background polling and performs the first reconcile
// synchronously so routing converges immediately after boot.
func (r *Runtime) Start() {
	if r == nil {
		return
	}
	if r.Puller != nil {
		r.Puller.Start()
	}
	r.Reconcile()
}

// Reconcile applies the latest snapshot to the gateway. The pull loop already
// does this after every successful pull; the exported form lets a caller force
// convergence without waiting for the next tick.
func (r *Runtime) Reconcile() {
	if r == nil || r.Reconciler == nil {
		return
	}
	r.Reconciler.Reconcile(context.Background())
}

// Stop drains the pull loop and removes the runtime credentials this package
// owns, so a restart never inherits stale routing rows.
func (r *Runtime) Stop() {
	if r == nil {
		return
	}
	if r.Puller != nil {
		r.Puller.Stop()
	}
	if r.Reconciler != nil {
		r.Reconciler.Release()
	}
}

// RuntimeOptions carries the dependencies Build needs.
type RuntimeOptions struct {
	// Config is the gateway configuration; the OpenAI-compatible executor uses it
	// for global proxy and payload rules. May be nil.
	Config *config.Config
	// Manager is the auth manager that owns credentials and executors.
	Manager *coreauth.Manager
	// Lookup resolves ProviderPrice.AuthRef to local credential material.
	Lookup CredentialLookup
	// EndpointPolicy validates delivered Endpoints. Zero value is the
	// production-safe default (https + public addresses only).
	EndpointPolicy EndpointPolicy
}

// Build constructs the runtime components for a running service.
//
// It returns nil when the channel is disabled or under-configured, so the caller
// skips installing the router host and the gateway keeps its default routing.
func Build(opts RuntimeOptions) *Runtime {
	if !enabledFromEnv() {
		logger.Debug("KEEPER arbitrage channel disabled (set " + EnvEnabled + "=true to enable)")
		return nil
	}
	keeperURL, keeperKey := keeperFromEnv()
	if keeperURL == "" || keeperKey == "" {
		logger.Warn("KEEPER arbitrage channel disabled (KEEPER URL and management key are both required)")
		return nil
	}
	if opts.Manager == nil {
		logger.Warn("KEEPER arbitrage channel disabled (auth manager unavailable)")
		return nil
	}
	if opts.Lookup == nil {
		logger.Warn("KEEPER arbitrage channel disabled (no credential resolver configured for auth_ref)")
		return nil
	}

	store := NewStore()
	reconciler := NewReconcilerWithConfig(opts.Manager, store, opts.Lookup, opts.Config)
	reconciler.SetEndpointPolicy(opts.EndpointPolicy)

	puller := NewPuller(store, PullerOptions{
		KeeperURL:       keeperURL,
		ManagementKey:   keeperKey,
		Interval:        intervalFromEnv(),
		StalenessFactor: stalenessFactorFromEnv(),
		AfterSync: func() {
			reconciler.Reconcile(context.Background())
		},
	})

	return &Runtime{
		Store:      store,
		Puller:     puller,
		Router:     NewRouterHost(store, time.Now, reconciler.ProviderKeyFor),
		Reconciler: reconciler,
	}
}

// ServerOption installs the arbitration model-router host on the constructed
// server through the pre-reserved WithRouterConfigurator hook, which avoids
// touching any upstream handler or route file. It returns nil for a nil runtime
// so callers can skip it and keep default behaviour.
//
// It also exposes a read-only status endpoint so an operator can verify that the
// upstream target really follows KEEPER's decisions (the acceptance evidence for
// "decision changed → upstream target changed").
func ServerOption(runtime *Runtime) api.ServerOption {
	if runtime == nil || runtime.Router == nil {
		return nil
	}
	return api.WithRouterConfigurator(func(_ *gin.Engine, baseHandler *handlers.BaseAPIHandler, _ *config.Config) {
		if baseHandler != nil {
			baseHandler.SetModelRouterHost(runtime.Router)
			logger.Info("arbitrage: model router host installed (upstream target follows KEEPER decisions)")
		}
	})
}

// StatusRoutePath is the read-only arbitration status endpoint. It is mounted by
// the api package inside the authenticated /v0/management group (see
// api.WithArbitrageStatusHandler), so it inherits the standard management-key
// auth. It never exposes AuthRef or key material.
const StatusRoutePath = "/v0/management/arbitrage/status"

// StatusHandler returns the gin handler for the status endpoint. The api package
// registers it inside the authenticated management group; exposing it as a
// handler instead of self-registering on the engine keeps it behind auth.
func (r *Runtime) StatusHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		if r == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "arbitrage runtime unavailable"})
			return
		}
		pulls, failures := uint64(0), uint64(0)
		if r.Puller != nil {
			pulls, failures = r.Puller.Counters()
		}
		stats := r.Store.Stats(time.Now())
		stats.Pulls = pulls
		stats.Failures = failures
		c.JSON(http.StatusOK, gin.H{
			"enabled":     true,
			"snapshot":    stats,
			"decisions":   r.Store.Views(),
			"credentials": r.Reconciler.AppliedCount(),
		})
	}
}

// StatusServerOption wires the status handler into the authenticated management
// group. Returns nil for a nil runtime.
func StatusServerOption(runtime *Runtime) api.ServerOption {
	if runtime == nil {
		return nil
	}
	return api.WithArbitrageStatusHandler(runtime.StatusHandler())
}

// ProviderKeyForDecision exposes the provider-key mapping for tests and callers
// that need to reason about which key a decision targets.
func ProviderKeyForDecision(decision Decision) string {
	return providerKeyForDecision(decision)
}

// enabledFromEnv resolves the enable switch.
func enabledFromEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvEnabled))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// keeperFromEnv resolves the KEEPER control-plane connection.
func keeperFromEnv() (url, key string) {
	url = strings.TrimSpace(os.Getenv(EnvKeeperURL))
	if url == "" {
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

func intervalFromEnv() time.Duration {
	seconds := envPositiveInt64(os.Getenv(EnvIntervalSec), int64(DefaultInterval/time.Second))
	return time.Duration(seconds) * time.Second
}

func stalenessFactorFromEnv() int {
	return int(envPositiveInt64(os.Getenv(EnvStalenessFactor), int64(DefaultStalenessFactor)))
}

func envPositiveInt64(value string, fallback int64) int64 {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

// deriveKeeperURL replaces the port of a CPA base URL with 4320 (KEEPER's
// default). Only a dev/local fallback; production should set EnvKeeperURL.
func deriveKeeperURL(base string) string {
	trimmed := strings.TrimRight(base, "/")
	idx := strings.LastIndex(trimmed, ":")
	if idx < 0 {
		return trimmed + ":4320"
	}
	return trimmed[:idx] + ":4320"
}
