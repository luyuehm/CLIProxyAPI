package contentfilter

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/api"
)

// Environment variables controlling the realtime content filter. All are
// optional; sensible defaults point at the KEEPER deployment used by the
// gateway (container enterprise-keeper-cpa-usage-keeper-1, volume path
// /var/lib/docker/volumes/enterprise-keeper_keeper-data/_data).
const (
	// EnvEnabled forces the filter on/off. "1"|"true"|"yes"|"on" enables,
	// "0"|"false"|"no"|"off" disables. Unset means auto-detect.
	EnvEnabled = "CPA_CONTENT_FILTER_ENABLED"
	// EnvHostDB overrides the host-visible KEEPER database path. When set and
	// readable, rules are read directly (read-only) from the mounted volume.
	EnvHostDB = "CPA_CONTENT_FILTER_KEEPER_HOST_DB"
	// EnvContainer overrides the KEEPER container name used for `docker cp`
	// (the fallback channel when the host volume path is unavailable).
	EnvContainer = "CPA_CONTENT_FILTER_KEEPER_CONTAINER"
	// EnvDBPath overrides the in-container SQLite database path.
	EnvDBPath = "CPA_CONTENT_FILTER_KEEPER_DB_PATH"
	// EnvIntervalSec overrides the rule refresh interval in seconds (default 30).
	EnvIntervalSec = "CPA_CONTENT_FILTER_INTERVAL_SECONDS"
)

// ServerOption returns an api.ServerOption that installs the realtime content
// filter middleware, constructing the rule syncer (initial load + background
// polling) at server construction time. It returns nil when the filter is
// disabled by configuration and no KEEPER source is reachable — callers should
// skip the option in that case.
//
// The returned option mounts through the pre-reserved api.WithMiddleware()
// extension point; it does not modify any upstream handler or core file.
func ServerOption() api.ServerOption {
	if !enabledFromEnv() {
		logger.Warn("content filter disabled (set " + EnvEnabled + "=true and configure a KEEPER source to enable)")
		return nil
	}
	opts := optsFromEnv()
	syncer, err := NewSyncer(opts)
	if err != nil {
		logger.WithError(err).Warn("content filter not installed: cannot create syncer")
		return nil
	}
	// Background polling hot-reloads rules within one RefreshInterval without
	// restarting the gateway.
	syncer.Start()
	engine := NewEngine(true) // outbound PII uses partial masking
	mw := NewMiddleware(syncer, engine)
	return api.WithMiddleware(mw.Handler())
}

// enabledFromEnv resolves the enable switch. An explicit env value wins;
// otherwise the filter auto-enables when a KEEPER source is configured (host
// volume path present, or an explicit container name) so bare test and dev
// environments stay quiet.
func enabledFromEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvEnabled))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	host := strings.TrimSpace(os.Getenv(EnvHostDB))
	if host == "" {
		host = DefaultHostVolumeDBPath
	}
	if _, err := os.Stat(host); err == nil {
		return true
	}
	return strings.TrimSpace(os.Getenv(EnvContainer)) != ""
}

// optsFromEnv builds the syncer options from environment configuration with
// the KEEPER defaults applied.
func optsFromEnv() SyncerOptions {
	opts := DefaultSyncerOptions()
	if h := strings.TrimSpace(os.Getenv(EnvHostDB)); h != "" {
		opts.HostDBPath = h
	}
	if c := strings.TrimSpace(os.Getenv(EnvContainer)); c != "" {
		opts.ContainerName = c
	}
	if db := strings.TrimSpace(os.Getenv(EnvDBPath)); db != "" {
		opts.ContainerDBPath = db
	}
	if s := strings.TrimSpace(os.Getenv(EnvIntervalSec)); s != "" {
		if sec, err := strconv.Atoi(s); err == nil && sec > 0 {
			opts.RefreshInterval = time.Duration(sec) * time.Second
		}
	}
	return opts
}