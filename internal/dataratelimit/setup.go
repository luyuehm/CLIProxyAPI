package dataratelimit

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/api"
)

// Environment variables controlling the data-plane rate limiter and KEEPER
// circuit breaker. All are optional; the feature is off by default so the
// gateway behaves exactly as upstream when unconfigured.
const (
	// EnvEnabled forces the rate limiter on/off. "1"|"true"|"yes"|"on"
	// enables, "0"|"false"|"no"|"off" disables. Unset means disabled.
	EnvEnabled = "CPA_RATE_LIMIT_ENABLED"
	// EnvKeeperURL is the KEEPER control-plane base URL for the quota-status
	// pull (e.g. http://127.0.0.1:4320). When set alongside EnvKeeperKey the
	// circuit breaker is active.
	EnvKeeperURL = "CPA_RATE_LIMIT_KEEPER_URL"
	// EnvKeeperKey is the shared X-CPA-Management-Key used to authenticate the
	// CPA-to-KEEPER quota-status pull.
	EnvKeeperKey = "CPA_RATE_LIMIT_KEEPER_KEY"
	// EnvIntervalSec overrides the quota-status pull interval in seconds
	// (default 5).
	EnvIntervalSec = "CPA_RATE_LIMIT_INTERVAL_SECONDS"
	// EnvDefaultRPM overrides the default per-key RPM for keys KEEPER has not
	// registered (default 0 = unlimited).
	EnvDefaultRPM = "CPA_RATE_LIMIT_DEFAULT_RPM"
	// EnvDefaultConcurrency overrides the default per-key max concurrency for
	// keys KEEPER has not registered (default 0 = unlimited).
	EnvDefaultConcurrency = "CPA_RATE_LIMIT_DEFAULT_CONCURRENCY"
)

// ServerOption returns an api.ServerOption that installs the data-plane rate
// limiter middleware, constructing the in-memory limiter and the KEEPER
// blacklist puller at server construction time. It returns nil when the
// feature is disabled by configuration, so callers should skip the option in
// that case.
//
// The returned option mounts through the pre-reserved api.WithMiddleware()
// extension point; it does not modify any upstream handler or core file.
func ServerOption() api.ServerOption {
	if !enabledFromEnv() {
		logger.Warn("data-plane rate limiter disabled (set " + EnvEnabled + "=true to enable)")
		return nil
	}

	defaultRPM, defaultConcurrency := defaultsFromEnv()

	limiter := NewLimiter(LimiterOptions{
		DefaultRPM:         defaultRPM,
		DefaultConcurrency: defaultConcurrency,
	})

	var blacklist *Blacklist
	if keeperURL, keeperKey := keeperFromEnv(); keeperURL != "" && keeperKey != "" {
		blacklist = NewBlacklist(BlacklistOptions{
			KeeperURL:     keeperURL,
			ManagementKey: keeperKey,
			Interval:      intervalFromEnv(),
		})
		blacklist.Start()
	}

	mw := NewMiddleware(MiddlewareOptions{
		Limiter:   limiter,
		Blacklist: blacklist,
	})
	return api.WithMiddleware(mw.Handler())
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

// defaultsFromEnv reads the default per-key RPM and concurrency.
func defaultsFromEnv() (rpm, concurrency int64) {
	rpm = envPositiveInt64(os.Getenv(EnvDefaultRPM), 0)
	concurrency = envPositiveInt64(os.Getenv(EnvDefaultConcurrency), 0)
	return rpm, concurrency
}

// keeperFromEnv resolves the KEEPER control-plane connection.
func keeperFromEnv() (url, key string) {
	return strings.TrimSpace(os.Getenv(EnvKeeperURL)), strings.TrimSpace(os.Getenv(EnvKeeperKey))
}

// intervalFromEnv reads the pull interval override in seconds.
func intervalFromEnv() time.Duration {
	seconds := envPositiveInt64(os.Getenv(EnvIntervalSec), int64(defaultQuotaStatusInterval/time.Second))
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
