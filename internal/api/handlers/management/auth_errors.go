package management

import (
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// Management auth failure reasons.
//
// The Management API used to answer every access failure with a free-form
// English sentence. A client could only string-match that sentence to tell a
// missing key from a disabled remote-management toggle, and the raw text leaked
// implementation detail ("IP banned due to too many failed attempts") straight
// into the browser.
//
// Each failure now carries a stable machine-readable code in the `error` field,
// so the dashboard maps codes to friendly localized prompts. Diagnostic detail
// (client IP, retry window) is written to the server log instead.
const (
	// AuthReasonMissingKey means no management key was supplied with the request.
	AuthReasonMissingKey = "management_key_missing"
	// AuthReasonInvalidKey means the supplied management key did not match.
	AuthReasonInvalidKey = "management_key_invalid"
	// AuthReasonKeyNotConfigured means no server-side key exists to compare against.
	AuthReasonKeyNotConfigured = "management_key_not_configured"
	// AuthReasonRemoteDisabled means remote management is off for a non-local client.
	AuthReasonRemoteDisabled = "remote_management_disabled"
	// AuthReasonRateLimited means the client IP is temporarily banned after repeated failures.
	AuthReasonRateLimited = "too_many_failed_attempts"
)

// AuthFailure describes one rejected management request.
type AuthFailure struct {
	// Status is the HTTP status sent to the client.
	Status int
	// Reason is the stable machine-readable code.
	Reason string
	// RetryAfter is the remaining ban window, meaningful only for AuthReasonRateLimited.
	RetryAfter time.Duration
}

// PublicMessage returns the short, non-technical text sent alongside Reason.
// It names no provider, key value or infrastructure detail so it is safe to
// surface to a browser.
func (f AuthFailure) PublicMessage() string {
	switch f.Reason {
	case AuthReasonMissingKey:
		return "management key required"
	case AuthReasonInvalidKey:
		return "management key rejected"
	case AuthReasonKeyNotConfigured:
		return "management key is not configured on the server"
	case AuthReasonRemoteDisabled:
		return "remote management is disabled"
	case AuthReasonRateLimited:
		return "too many failed attempts, retry later"
	default:
		return "access denied"
	}
}

// legacyMessage preserves the exact strings this package returned before the
// codes existed. Internal callers (the Redis queue protocol) prefix-match them,
// so they must not change.
func (f AuthFailure) legacyMessage() string {
	switch f.Reason {
	case AuthReasonMissingKey:
		return "missing management key"
	case AuthReasonInvalidKey:
		return "invalid management key"
	case AuthReasonKeyNotConfigured:
		return "remote management key not set"
	case AuthReasonRemoteDisabled:
		return "remote management disabled"
	case AuthReasonRateLimited:
		return fmt.Sprintf("IP banned due to too many failed attempts. Try again in %s", f.RetryAfter.Round(time.Second))
	default:
		return "access denied"
	}
}

// logDetails returns the diagnostic context written to the server log. It is
// deliberately richer than what the client receives: operators need the client
// IP and the reason, callers must not.
func (f AuthFailure) logDetails(clientIP string, localClient bool, keyProvided bool) log.Fields {
	fields := log.Fields{
		"reason":       f.Reason,
		"status":       f.Status,
		"client_ip":    clientIP,
		"local_client": localClient,
		"key_provided": keyProvided,
		"at":           time.Now().UTC().Format(time.RFC3339),
	}
	if f.RetryAfter > 0 {
		fields["retry_after_seconds"] = int(f.RetryAfter.Round(time.Second).Seconds())
	}
	return fields
}

// Shared failures for the reasons that carry no variable detail.
var (
	authFailureMissingKey       = AuthFailure{Status: http.StatusUnauthorized, Reason: AuthReasonMissingKey}
	authFailureInvalidKey       = AuthFailure{Status: http.StatusUnauthorized, Reason: AuthReasonInvalidKey}
	authFailureKeyNotConfigured = AuthFailure{Status: http.StatusForbidden, Reason: AuthReasonKeyNotConfigured}
	authFailureRemoteDisabled   = AuthFailure{Status: http.StatusForbidden, Reason: AuthReasonRemoteDisabled}
)

// managementAuthResponse is the JSON envelope returned to rejected clients.
type managementAuthResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
	// RetryAfter is a whole number of seconds; omitted for every reason but rate limiting.
	RetryAfter int `json:"retry_after,omitempty"`
}

// abortRequest writes the mapped status and envelope and logs the diagnostic
// detail. The caller must hold no locks.
func (f AuthFailure) abortRequest(c *gin.Context, clientIP string, localClient bool, keyProvided bool) {
	log.WithFields(f.logDetails(clientIP, localClient, keyProvided)).Warn("management: access denied")
	body := managementAuthResponse{Error: f.Reason, Message: f.PublicMessage()}
	if f.RetryAfter > 0 {
		body.RetryAfter = int(f.RetryAfter.Round(time.Second).Seconds())
		if body.RetryAfter < 1 {
			body.RetryAfter = 1
		}
	}
	c.AbortWithStatusJSON(f.Status, body)
}
