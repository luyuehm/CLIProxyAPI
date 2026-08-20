package license

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestLocalFormatValid(t *testing.T) {
	tests := []struct {
		key   string
		valid bool
	}{
		{"CLIPA-abcdefghijklmnop", true},
		{"clipa-abcdefghijklmnop", true},
		{"CLIPA-a1b2c3d4e5f6g7h8i9j0k1l2m3n4o5p6q7r8s9t0u1v2w3x4y5z6", true},
		{"", false},
		{"CLIPA-", false},
		{"CLIPA-ab", false},
		{"INVALID-abcdefghijklmnop", false},
		{"CLIPA-abc def ghi", false},
		{"CLIPA-abc!def@ghi", false},
		{"CLIPA-abcABC123-_...", true},
		{"CLIPA-" + string(make([]byte, 600)), false},
	}
	for _, tt := range tests {
		got := localFormatValid(tt.key)
		if got != tt.valid {
			t.Errorf("localFormatValid(%q) = %v, want %v", tt.key, got, tt.valid)
		}
	}
}

func TestMaskKey(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"CLIPA-abcdefghijklmnop", "CLIPA-***mnop"},
		{"abc", "***"},
		{"CLIPA-1234567890abc", "CLIPA-***0abc"},
	}
	for _, tt := range tests {
		got := maskKey(tt.input)
		if got != tt.want {
			t.Errorf("maskKey(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestValidateNoKey(t *testing.T) {
	info := Validate(context.Background(), Options{})
	if info.Status != StatusInvalid {
		t.Errorf("expected StatusInvalid, got %s", info.Status)
	}
	if info.Message == "" {
		t.Error("expected non-empty message")
	}
}

func TestValidateLocalFormat(t *testing.T) {
	info := Validate(context.Background(), Options{
		Key: "bad-key-format",
	})
	if info.Status != StatusInvalid {
		t.Errorf("expected StatusInvalid, got %s", info.Status)
	}
}

func TestValidateRemoteValid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(LicenseServerResponse{
			Valid:   true,
			Message: "valid",
		})
	}))
	defer srv.Close()

	info := Validate(context.Background(), Options{
		Key:        "CLIPA-abcdefghijklmnop",
		ServerURL:  srv.URL,
		HTTPClient: defaultHTTPClient(5 * time.Second),
	})
	if info.Status != StatusValid {
		t.Errorf("expected StatusValid, got %s: %s", info.Status, info.Message)
	}
}

func TestValidateRemoteInvalid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(LicenseServerResponse{
			Valid:   false,
			Message: "invalid key",
		})
	}))
	defer srv.Close()

	info := Validate(context.Background(), Options{
		Key:        "CLIPA-abcdefghijklmnop",
		ServerURL:  srv.URL,
		HTTPClient: defaultHTTPClient(5 * time.Second),
	})
	if info.Status != StatusInvalid {
		t.Errorf("expected StatusInvalid, got %s: %s", info.Status, info.Message)
	}
}

func TestValidateRemoteExpired(t *testing.T) {
	expired := time.Now().Add(-24 * time.Hour).Format(time.RFC3339)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(LicenseServerResponse{
			Valid:     false,
			ExpiresAt: expired,
			Message:   "license expired",
		})
	}))
	defer srv.Close()

	info := Validate(context.Background(), Options{
		Key:        "CLIPA-abcdefghijklmnop",
		ServerURL:  srv.URL,
		HTTPClient: defaultHTTPClient(5 * time.Second),
	})
	if info.Status != StatusExpired {
		t.Errorf("expected StatusExpired, got %s: %s", info.Status, info.Message)
	}
	if info.ExpiresAt == nil {
		t.Error("expected non-nil ExpiresAt")
	}
}

func TestValidateRemoteError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	info := Validate(context.Background(), Options{
		Key:        "CLIPA-abcdefghijklmnop",
		ServerURL:  srv.URL,
		HTTPClient: defaultHTTPClient(5 * time.Second),
	})
	if info.Status != StatusError {
		t.Errorf("expected StatusError, got %s: %s", info.Status, info.Message)
	}
}

func TestValidateRemoteTimeout(t *testing.T) {
	// Create a server that never responds
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second)
		_ = json.NewEncoder(w).Encode(LicenseServerResponse{Valid: true})
	}))
	defer srv.Close()

	info := Validate(context.Background(), Options{
		Key:       "CLIPA-abcdefghijklmnop",
		ServerURL: srv.URL,
		HTTPClient: &http.Client{
			Timeout: 100 * time.Millisecond,
		},
	})
	if info.Status != StatusError {
		t.Errorf("expected StatusError, got %s: %s", info.Status, info.Message)
	}
}

func TestOfflineCache(t *testing.T) {
	cacheFile := t.TempDir() + "/license-cache.json"

	// First call: server returns valid
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(LicenseServerResponse{
			Valid: true,
		})
	}))
	defer srv.Close()

	info := Validate(context.Background(), Options{
		Key:              "CLIPA-abcdefghijklmnop",
		ServerURL:        srv.URL,
		OfflineCachePath: cacheFile,
	})
	if info.Status != StatusValid {
		t.Fatalf("expected StatusValid, got %s", info.Status)
	}

	// Now close the server and validate again — should use cache
	srv.Close()
	info2 := Validate(context.Background(), Options{
		Key:              "CLIPA-abcdefghijklmnop",
		ServerURL:        "http://localhost:19099",
		OfflineCachePath: cacheFile,
	})
	if info2.Status != StatusValid {
		t.Errorf("expected StatusValid from cache, got %s: %s", info2.Status, info2.Message)
	}
}

func TestEnforcementStartupCheck(t *testing.T) {
	// Test block mode with valid key
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(LicenseServerResponse{Valid: true})
	}))
	defer srv.Close()

	os.Setenv(EnvLicenseKey, "CLIPA-abcdefghijklmnop")
	os.Setenv(EnvLicenseMode, "block")
	defer os.Unsetenv(EnvLicenseKey)
	defer os.Unsetenv(EnvLicenseMode)

	result := StartupCheck(context.Background(), Options{
		ServerURL: srv.URL,
	})
	if !result {
		t.Error("expected StartupCheck to return true for valid key in block mode")
	}
}

func TestReadEnforcementMode(t *testing.T) {
	os.Unsetenv(EnvLicenseMode)

	mode := readEnforcementMode()
	if mode != ModeBlock {
		t.Errorf("expected ModeBlock, got %s", mode)
	}

	os.Setenv(EnvLicenseMode, "ratelimit")
	defer os.Unsetenv(EnvLicenseMode)

	mode = readEnforcementMode()
	if mode != ModeRateLimit {
		t.Errorf("expected ModeRateLimit, got %s", mode)
	}
}

func TestIsExpired(t *testing.T) {
	future := time.Now().Add(24 * time.Hour)
	if isExpired(&future) {
		t.Error("expected future time to not be expired")
	}

	past := time.Now().Add(-24 * time.Hour)
	if !isExpired(&past) {
		t.Error("expected past time to be expired")
	}

	if isExpired(nil) {
		t.Error("expected nil to not be expired")
	}
}

func TestParseLicenseTime(t *testing.T) {
	_, err := parseLicenseTime("")
	if err == nil {
		t.Error("expected error for empty string")
	}

	_, err = parseLicenseTime("invalid")
	if err == nil {
		t.Error("expected error for invalid format")
	}

	parsed, err := parseLicenseTime("2026-01-01T00:00:00Z")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if parsed.IsZero() {
		t.Error("expected non-zero time")
	}
}

func TestSetDegraded(t *testing.T) {
	SetDegraded(false)
	if IsDegraded() {
		t.Error("expected false")
	}

	SetDegraded(true)
	if !IsDegraded() {
		t.Error("expected true")
	}

	SetDegraded(false)
}
