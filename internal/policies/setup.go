package policies

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/api"
)

// Environment variables controlling the KEEPER policy puller. All are optional;
// the feature is off by default so the gateway behaves exactly as upstream when
// unconfigured.
const (
	// EnvEnabled forces the policy channel on/off. "1"|"true"|"yes"|"on"
	// enables, "0"|"false"|"no"|"off" disables. Unset means disabled.
	EnvEnabled = "CPA_POLICIES_ENABLED"
	// EnvKeeperURL is the KEEPER control-plane base URL (e.g.
	// http://127.0.0.1:4320). When set alongside EnvKeeperKey the puller is
	// active. If empty, it is derived from CPA_BASE_URL (port → 4320).
	EnvKeeperURL = "CPA_POLICIES_KEEPER_URL"
	// EnvKeeperKey is the shared X-CPA-Management-Key used to authenticate the
	// CPA-to-KEEPER policy pull.
	EnvKeeperKey = "CPA_POLICIES_KEEPER_KEY"
	// EnvIntervalSec overrides the policy pull interval in seconds (default 5).
	EnvIntervalSec = "CPA_POLICIES_INTERVAL_SECONDS"
	// EnvManagementKey is reused from the content-filter audit channel; it is
	// the same shared key as EnvKeeperKey. Setting either enables the pull.
	EnvManagementKey = "CPA_CONTENT_FILTER_AUDIT_KEEPER_KEY"
)

// ServerOption returns an api.ServerOption that installs the KEEPER policy
// puller, constructing the in-memory store and the background poller at server
// construction time. It returns nil when the channel is disabled by
// configuration, so callers should skip the option in that case.
//
// The returned option mounts through the pre-reserved api.WithMiddleware()
// extension point with an admission middleware that enforces the delivered
// per-key RPM/concurrency policies. It does not modify any upstream handler or
// core file.
func ServerOption() api.ServerOption {
	if !enabledFromEnv() {
		logger.Warn("KEEPER policy channel disabled (set " + EnvEnabled + "=true and configure a KEEPER URL + management key to enable)")
		return nil
	}
	keeperURL, keeperKey := keeperFromEnv()
	if keeperURL == "" || keeperKey == "" {
		logger.Warn("KEEPER policy channel disabled (KEEPER URL and management key are both required)")
		return nil
	}
	store := NewStore()
	puller := NewPuller(store, PullerOptions{
		KeeperURL:     keeperURL,
		ManagementKey: keeperKey,
		Interval:      intervalFromEnv(),
	})
	puller.Start()

	mw := NewMiddleware(MiddlewareOptions{Store: store})
	return api.WithMiddleware(mw.Handler())
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
		// Dev/默认：从 CPA_BASE_URL 推导 4320 端口（与内容过滤通道一致）。
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