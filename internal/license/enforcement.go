package license

import (
	"context"
	"os"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// GlobalDegraded is a package-level flag that indicates the server should
// operate in degraded mode (e.g., rate-limited proxy endpoints).
var GlobalDegraded atomicBool

// atomicBool is a simple boolean wrapper for clean atomics API.
type atomicBool struct {
	mu sync.RWMutex
	v  bool
}

func (a *atomicBool) Store(v bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.v = v
}

func (a *atomicBool) Load() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.v
}

// SetDegraded globally enables or disables degraded mode.
func SetDegraded(degraded bool) {
	GlobalDegraded.Store(degraded)
}

// IsDegraded returns the current degraded mode state.
func IsDegraded() bool {
	return GlobalDegraded.Load()
}

// StartupCheck performs license validation at server startup and returns
// whether the server should continue starting. If the license is invalid and
// mode is ModeBlock, it logs an error and returns false. If mode is ModeRateLimit,
// it sets the degraded flag and returns true.
func StartupCheck(ctx context.Context, opts Options) bool {
	info := Validate(ctx, opts)

	log.WithFields(log.Fields{
		"status": info.Status,
		"mode":   info.Mode,
		"key":    info.Key,
		"expiry": formatExpiry(info.ExpiresAt),
	}).Info("license check")

	switch info.Mode {
	case ModeNone:
		return true
	case ModeBlock:
		if !info.IsValid() {
			log.Errorf("license validation failed: %s (mode=block). server will not start.", info.Message)
			return false
		}
		log.Infof("license valid: %s", info.Message)
		return true
	case ModeRateLimit:
		if !info.IsValid() {
			log.Warnf("license validation failed: %s (mode=ratelimit). server starting in degraded mode.", info.Message)
			SetDegraded(true)
			return true
		}
		log.Infof("license valid: %s", info.Message)
		return true
	default:
		// Unknown mode defaults to block for safety.
		if !info.IsValid() {
			log.Errorf("license validation failed: %s (unknown mode, defaulting to block). server will not start.", info.Message)
			return false
		}
		return true
	}
}

func formatExpiry(t *time.Time) string {
	if t == nil {
		return "unknown"
	}
	return t.Format(time.RFC3339)
}

// EnvVarsDoc returns a documentation string for the license env vars.
// Used in .env.example and startup logs.
func EnvVarsDoc() string {
	return `# Enterprise License Configuration
# LICENSE_KEY is required for enterprise deployment.
# Get a license key from your provider.
# LICENSE_KEY=CLIPA-xxxxxxxxxxxx

# LICENSE_MODE controls enforcement: block (refuse to start) or ratelimit (degrade).
# Default: block
# LICENSE_MODE=block

# LICENSE_SERVER_URL overrides the default license verification endpoint.
# Default: https://license.cliproxyapi.com/api/v1/verify
# LICENSE_SERVER_URL=https://your-license-server.example.com/api/v1/verify
`
}

// CheckLicenseMiddleware is a convenience function for the server startup flow.
// It reads the environment and validates the license, performing the appropriate
// enforcement action. Returns true if the server should start.
func CheckLicenseMiddleware(ctx context.Context) bool {
	envKey := os.Getenv(EnvLicenseKey)
	if envKey == "" {
		// No license key at all — check if this is enterprise deployment
		// by looking for DEPLOY=cloud or enterprise markers
		deploy := os.Getenv("DEPLOY")
		if deploy == "cloud" {
			log.Warn("cloud deploy mode with no license key; server will start, but check license settings")
			return true
		}
		// For non-enterprise deployments, no license is required
		log.Info("no license key configured; running in community mode")
		return true
	}

	return StartupCheck(ctx, Options{
		Key:              envKey,
		OfflineCachePath: defaultCachePath(),
	})
}
