package auditchain

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/api"
	log "github.com/sirupsen/logrus"
)

// Environment variables controlling the audit chain of responsibility. All
// are optional; the feature is off by default so an unconfigured gateway
// mounts no routes and opens no database.
const (
	// EnvEnabled forces the channel on/off. "1"|"true"|"yes"|"on" enables.
	// Unset means disabled.
	EnvEnabled = "CPA_AUDIT_CHAIN_ENABLED"
	// EnvDBPath overrides the local SQLite audit database path. The default
	// is <tempdir>/cpa-audit-chain/audit.db so a fresh deployment writes
	// to local storage (the distributed storage of the log file is
	// explicitly out of scope for this channel).
	EnvDBPath = "CPA_AUDIT_CHAIN_DB_PATH"
	// EnvKeyHex is a hex-encoded 32-byte key. When set, model context
	// snapshots (model_input / model_output) are encrypted at rest with
	// AES-GCM and flagged snapshot_recorded=true. When unset, snapshots
	// are dropped fail-closed and records carry no model context.
	EnvKeyHex = "CPA_AUDIT_CHAIN_KEY_HEX"
)

// DefaultDBDir is where the audit database is created when EnvDBPath is
// unset.
const DefaultDBDir = "cpa-audit-chain"

// logger is the package-level logger.
var logger = log.WithField("component", "auditchain")

// enabledFromEnv resolves the enable switch; unset means off.
func enabledFromEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvEnabled))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// dbPathFromEnv resolves the database location.
func dbPathFromEnv() string {
	if v := strings.TrimSpace(os.Getenv(EnvDBPath)); v != "" {
		return v
	}
	return filepath.Join(os.TempDir(), DefaultDBDir, "audit.db")
}

// keyFromEnv resolves the snapshot encryption key. The returned slice is
// nil when the channel is unset or malformed; a malformed key is a
// configuration error, not a silent fallback, so the caller disables
// capture (fail-closed) rather than writing plaintext.
func keyFromEnv() []byte {
	v := strings.TrimSpace(os.Getenv(EnvKeyHex))
	if v == "" {
		return nil
	}
	key, err := hex.DecodeString(v)
	if err != nil || len(key) != 32 {
		logger.Warn("audit chain: " + EnvKeyHex + " is not a hex-encoded 32-byte key; context capture disabled (fail-closed)")
		return nil
	}
	return key
}

// ServerOption returns an api.ServerOption that registers the audit-chain
// endpoints under the authenticated /v0/management group:
//
//	GET  /v0/management/audit/operations/:id
//	POST /v0/management/audit/events
//
// It returns nil when the channel is disabled or the database cannot be
// opened, so callers skip the option and the gateway runs exactly as
// before. Snapshot capture is enabled only when a valid key is configured
// (EnvKeyHex); without one the handlers still serve the chain and the
// "query by operation ID" contract, just without model context.
func ServerOption() api.ServerOption {
	if !enabledFromEnv() {
		logger.Debug("audit chain channel disabled (set " + EnvEnabled + "=true to enable)")
		return nil
	}
	dbPath := dbPathFromEnv()
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		logger.WithError(err).Warn("audit chain: cannot create database directory; channel disabled")
		return nil
	}
	store, err := NewStore(dbPath, nil)
	if err != nil {
		logger.WithError(err).Warn("audit chain: cannot open database; channel disabled")
		return nil
	}
	key := keyFromEnv()
	logger.WithFields(map[string]any{
		"db":       dbPath,
		"snapshot": key != nil,
	}).Info("audit chain channel enabled")
	return api.WithAuditChainHandler(NewHandler(store, key).Handle)
}
