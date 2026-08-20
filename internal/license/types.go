// Package license provides enterprise license key validation and enforcement.
// It supports local format validation, remote verification against a license server,
// and configurable enforcement modes (block startup or degrade).
package license

import "time"

// LicenseStatus represents the current status of a license key.
type LicenseStatus string

const (
	StatusValid   LicenseStatus = "valid"
	StatusExpired LicenseStatus = "expired"
	StatusInvalid LicenseStatus = "invalid"
	StatusError   LicenseStatus = "error" // transient validation error
)

// EnforcementMode controls behavior when a license is invalid or missing.
type EnforcementMode string

const (
	// ModeBlock prevents the server from starting when the license is invalid.
	ModeBlock EnforcementMode = "block"
	// ModeRateLimit allows the server to start but with degraded performance.
	ModeRateLimit EnforcementMode = "ratelimit"
	// ModeNone disables license enforcement (open source / free tier).
	ModeNone EnforcementMode = "none"
)

// LicenseInfo holds the result of a license validation.
type LicenseInfo struct {
	// Key is the raw license key string.
	Key string `json:"key"`
	// Status is the current validation status.
	Status LicenseStatus `json:"status"`
	// ExpiresAt is the license expiry timestamp, if known.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// Message provides a human-readable description of the status.
	Message string `json:"message,omitempty"`
	// Features lists the enabled enterprise features.
	Features []string `json:"features,omitempty"`
	// Mode is the enforcement mode for this check.
	Mode EnforcementMode `json:"mode"`
}

// IsValid returns true when the license status is valid.
func (l *LicenseInfo) IsValid() bool {
	return l.Status == StatusValid
}

// IsExpired returns true when the license has expired.
func (l *LicenseInfo) IsExpired() bool {
	return l.Status == StatusExpired
}

// LicenseServerResponse is the expected response from the remote license server.
type LicenseServerResponse struct {
	// Valid indicates whether the license key is valid.
	Valid bool `json:"valid"`
	// ExpiresAt is the ISO 8601 expiry timestamp, if available.
	ExpiresAt string `json:"expires_at,omitempty"`
	// Message provides additional context from the server.
	Message string `json:"message,omitempty"`
	// Features lists the enabled enterprise features.
	Features []string `json:"features,omitempty"`
}

// DefaultLicenseServerURL is the default URL for the remote license server.
const DefaultLicenseServerURL = "https://license.cliproxyapi.com/api/v1/verify"

// EnvLicenseKey is the environment variable name for the license key.
const EnvLicenseKey = "LICENSE_KEY"

// EnvLicenseMode is the environment variable name for the enforcement mode.
const EnvLicenseMode = "LICENSE_MODE"

// EnvLicenseServerURL is the environment variable name for the license server URL.
const EnvLicenseServerURL = "LICENSE_SERVER_URL"
