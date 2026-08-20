package license

import (
	"context"
	"os"
	"strings"
	"time"
)

// Options configures the license validation behavior.
type Options struct {
	// Key is the license key to validate. If empty, reads from EnvLicenseKey.
	Key string
	// ServerURL is the remote license server endpoint. If empty, uses DefaultLicenseServerURL.
	ServerURL string
	// HTTPClient is the HTTP client for remote validation. If nil, uses a default client.
	HTTPClient HTTPDoer
	// Timeout for the remote validation request. Defaults to 10 seconds.
	Timeout time.Duration
	// OfflineCachePath is a file path to cache the last known good license info.
	// When the remote server is unreachable, the cached info is used with expiry checks.
	OfflineCachePath string
	// Mode is the enforcement mode. If empty, reads from EnvLicenseMode; defaults to ModeBlock.
	Mode EnforcementMode
}

// Defaults
const (
	defaultTimeout = 10 * time.Second
)

// Validate performs license validation: local format check, then remote verification.
// On transient network errors, falls back to cached validation if available.
func Validate(ctx context.Context, opts Options) *LicenseInfo {
	key := opts.Key
	if key == "" {
		key = os.Getenv(EnvLicenseKey)
	}

	mode := opts.Mode
	if mode == "" {
		mode = readEnforcementMode()
	}

	// No license key provided
	if strings.TrimSpace(key) == "" {
		info := &LicenseInfo{
			Key:     "",
			Status:  StatusInvalid,
			Message: "no license key provided; set " + EnvLicenseKey + " environment variable",
			Mode:    mode,
		}
		return info
	}

	key = strings.TrimSpace(key)

	// Local format validation
	if !localFormatValid(key) {
		return &LicenseInfo{
			Key:     maskKey(key),
			Status:  StatusInvalid,
			Message: "license key has invalid format",
			Mode:    mode,
		}
	}

	// Remote validation
	serverURL := opts.ServerURL
	if serverURL == "" {
		serverURL = os.Getenv(EnvLicenseServerURL)
	}
	if serverURL == "" {
		serverURL = DefaultLicenseServerURL
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	client := opts.HTTPClient
	if client == nil {
		client = defaultHTTPClient(timeout)
	}

	info := validateRemote(ctx, key, serverURL, client, mode)

	// On transient errors, try offline cache
	if info.Status == StatusError && opts.OfflineCachePath != "" {
		if cached := readCachedInfo(opts.OfflineCachePath); cached != nil {
			if cached.Status == StatusValid && !isExpired(cached.ExpiresAt) {
				cached.Message = "using cached license (remote server unreachable)"
				cached.Mode = mode
				return cached
			}
		}
		info.Message = "remote validation failed and no valid cached license available"
		return info
	}

	// Cache valid/expired responses for offline fallback
	if opts.OfflineCachePath != "" && (info.Status == StatusValid || info.Status == StatusExpired) {
		_ = writeCachedInfo(opts.OfflineCachePath, info)
	}

	return info
}

// localFormatValid performs a basic sanity check on the license key format.
// Key format: "CLIPA-" followed by base64url-encoded payload (minimum 10 chars).
func localFormatValid(key string) bool {
	if len(key) < 16 || len(key) > 512 {
		return false
	}
	if !strings.HasPrefix(key, "CLIPA-") && !strings.HasPrefix(key, "clipa-") {
		return false
	}
	// Check for valid characters after the prefix
	payload := key[6:]
	for _, c := range payload {
		if !isValidKeyChar(c) {
			return false
		}
	}
	return true
}

func isValidKeyChar(c rune) bool {
	return (c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') ||
		c == '-' || c == '_' || c == '.'
}

func maskKey(key string) string {
	if len(key) <= 10 {
		return "***"
	}
	return key[:6] + "***" + key[len(key)-4:]
}

func readEnforcementMode() EnforcementMode {
	mode := os.Getenv(EnvLicenseMode)
	switch EnforcementMode(mode) {
	case ModeBlock, ModeRateLimit:
		return EnforcementMode(mode)
	default:
		return ModeBlock
	}
}

func isExpired(t *time.Time) bool {
	if t == nil {
		return false
	}
	return time.Now().After(*t)
}

// ValidateConfig is a convenience function that validates a license key
// from the environment and returns the result. It uses defaults.
func ValidateConfig(ctx context.Context) *LicenseInfo {
	return Validate(ctx, Options{
		OfflineCachePath: defaultCachePath(),
	})
}

// defaultCachePath returns the default path for the license cache file.
func defaultCachePath() string {
	if home, err := os.UserHomeDir(); err == nil {
		return home + "/.cli-proxy-api-license-cache.json"
	}
	return ""
}
