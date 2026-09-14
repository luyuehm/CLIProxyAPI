package config

import (
	"strings"
	"time"
)

// Default offline-first routing values. Kept as package-level functions so tests
// and callers share one source of truth for the defaults.
const (
	// DefaultOfflineHealthCheckInterval is the default probe interval.
	DefaultOfflineHealthCheckInterval = 30 * time.Second
	// DefaultOfflineHealthCheckTimeout is the default per-probe timeout.
	DefaultOfflineHealthCheckTimeout = 3 * time.Second
	// DefaultOfflineHealthCheckPath is the default OpenAI-compatible probe path.
	DefaultOfflineHealthCheckPath = "/models"
)

// OfflineEnabled reports whether offline-first routing mode is enabled.
func (cfg *Config) OfflineEnabled() bool {
	return cfg != nil && cfg.Routing.Offline.Mode
}

// OfflinePreferLocal reports whether local endpoints take precedence over
// remote endpoints. Defaults to true when offline mode is enabled.
func (cfg *Config) OfflinePreferLocal() bool {
	if cfg == nil || !cfg.Routing.Offline.Mode {
		return false
	}
	if cfg.Routing.Offline.PreferLocal == nil {
		return true
	}
	return *cfg.Routing.Offline.PreferLocal
}

// OfflineAllowRemoteFallback reports whether remote endpoints are allowed as a
// fallback when no local endpoint is available. Defaults to false in offline
// mode; callers that only ever have remote endpoints must set it explicitly.
func (cfg *Config) OfflineAllowRemoteFallback() bool {
	if cfg == nil {
		return false
	}
	if cfg.Routing.Offline.AllowRemoteFallback == nil {
		// When offline mode is not explicitly enabled, remote fallback is the
		// default behavior (nothing is filtered).
		return !cfg.Routing.Offline.Mode
	}
	return *cfg.Routing.Offline.AllowRemoteFallback
}

// OfflineHealthCheckEnabled reports whether the proactive health-check loop is
// enabled. It is only meaningful (and only probes) when offline mode is on, but
// the knob can be enabled independently.
func (cfg *Config) OfflineHealthCheckEnabled() bool {
	return cfg != nil && cfg.Routing.Offline.HealthCheck.Enabled
}

// OfflineHealthCheckInterval parses the configured probe interval, falling back
// to the default.
func (cfg *Config) OfflineHealthCheckInterval() time.Duration {
	if cfg != nil {
		if raw := strings.TrimSpace(cfg.Routing.Offline.HealthCheck.Interval); raw != "" {
			if d, err := time.ParseDuration(raw); err == nil && d > 0 {
				return d
			}
		}
	}
	return DefaultOfflineHealthCheckInterval
}

// OfflineHealthCheckTimeout parses the configured per-probe timeout, falling
// back to the default.
func (cfg *Config) OfflineHealthCheckTimeout() time.Duration {
	if cfg != nil {
		if raw := strings.TrimSpace(cfg.Routing.Offline.HealthCheck.Timeout); raw != "" {
			if d, err := time.ParseDuration(raw); err == nil && d > 0 {
				return d
			}
		}
	}
	return DefaultOfflineHealthCheckTimeout
}

// OfflineHealthCheckPath returns the configured probe path (normalized to start
// with "/"), falling back to the default.
func (cfg *Config) OfflineHealthCheckPath() string {
	if cfg != nil {
		if raw := strings.TrimSpace(cfg.Routing.Offline.HealthCheck.Path); raw != "" {
			if strings.HasPrefix(raw, "/") {
				return raw
			}
			return "/" + raw
		}
	}
	return DefaultOfflineHealthCheckPath
}

// OfflineHealthCheckProbeOnlyLocal reports whether the health-check loop should
// probe only local endpoints. Defaults to true when offline mode is enabled.
func (cfg *Config) OfflineHealthCheckProbeOnlyLocal() bool {
	if cfg == nil {
		return false
	}
	if cfg.Routing.Offline.HealthCheck.ProbeOnlyLocal != nil {
		return *cfg.Routing.Offline.HealthCheck.ProbeOnlyLocal
	}
	return cfg.Routing.Offline.Mode
}
