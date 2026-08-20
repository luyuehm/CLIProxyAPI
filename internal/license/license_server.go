package license

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// HTTPDoer is the minimal HTTP client interface used for remote validation.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

func defaultHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout: 5 * time.Second,
			}).DialContext,
			MaxIdleConns:    10,
			IdleConnTimeout: 30 * time.Second,
		},
	}
}

// RemoteValidationError indicates the validation request failed.
type remoteValidationError struct {
	message string
}

func (e *remoteValidationError) Error() string {
	return e.message
}

// IsTransient reports whether an error is likely transient (network/unreachable).
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var urlErr *net.DNSError
	return errors.As(err, &urlErr) || strings.Contains(err.Error(), "connection refused") || strings.Contains(err.Error(), "timeout")
}

// validateRemote calls the license server to verify the key.
func validateRemote(ctx context.Context, key, serverURL string, client HTTPDoer, mode EnforcementMode) *LicenseInfo {
	info, err := callLicenseServer(ctx, key, serverURL, client)
	if err != nil {
		return &LicenseInfo{
			Key:     maskKey(key),
			Status:  StatusError,
			Message: "license server unreachable: " + err.Error(),
			Mode:    mode,
		}
	}

	base := &LicenseInfo{
		Key:      maskKey(key),
		Features: info.Features,
		Mode:     mode,
		Message:  normalizedMessage(info.Message),
	}

	expiresAt, parseErr := parseLicenseTime(info.ExpiresAt)
	if parseErr == nil {
		base.ExpiresAt = &expiresAt
	}

	if !info.Valid {
		// If the server returns an expiring license, distinguish expired vs invalid.
		if parseErr == nil && isExpired(&expiresAt) {
			base.Status = StatusExpired
			base.Message = "license key has expired"
		} else {
			base.Status = StatusInvalid
			if base.Message == "" {
				base.Message = "license key rejected by license server"
			}
		}
		return base
	}

	base.Status = StatusValid
	if base.Message == "" {
		base.Message = "license key is valid"
	}
	return base
}

// callLicenseServer sends the license key to the remote server and parses the response.
func callLicenseServer(ctx context.Context, key, serverURL string, client HTTPDoer) (*LicenseServerResponse, error) {
	body, err := json.Marshal(map[string]string{"license_key": key, "product": "cliproxyapi"})
	if err != nil {
		return nil, &remoteValidationError{message: fmt.Sprintf("marshal request: %v", err)}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, serverURL, bytes.NewReader(body))
	if err != nil {
		return nil, &remoteValidationError{message: fmt.Sprintf("build request: %v", err)}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "CLIProxyAPI/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return nil, &remoteValidationError{message: err.Error()}
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	var out LicenseServerResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, &remoteValidationError{message: fmt.Sprintf("decode license server response: %v", err)}
	}
	return &out, nil
}

// parseLicenseTime parses an ISO8601 license expiry timestamp.
func parseLicenseTime(raw string) (time.Time, error) {
	if strings.TrimSpace(raw) == "" {
		return time.Time{}, fmt.Errorf("empty timestamp")
	}
	return time.Parse(time.RFC3339, raw)
}

func normalizedMessage(msg string) string {
	return strings.TrimSpace(msg)
}
