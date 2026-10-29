package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"golang.org/x/crypto/bcrypt"
)

// newAuthTestHandler builds a Handler with the given remote-management settings.
// envSecret is left empty so AllowRemote/SecretKey are the only inputs.
func newAuthTestHandler(allowRemote bool, secretKey string) *Handler {
	return &Handler{
		cfg: &config.Config{RemoteManagement: config.RemoteManagement{
			AllowRemote: allowRemote,
			SecretKey:   secretKey,
		}},
		failedAttempts: make(map[string]*attemptInfo),
	}
}

// callMiddleware runs the auth middleware for one request and returns the recorder.
func callMiddleware(t *testing.T, h *Handler, clientIP, suppliedKey string) *httptest.ResponseRecorder {
	t.Helper()
	engine := gin.New()
	engine.GET("/v0/management/config", h.Middleware(), func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v0/management/config", nil)
	req.RemoteAddr = clientIP + ":12345"
	if suppliedKey != "" {
		req.Header.Set("X-Management-Key", suppliedKey)
	}
	engine.ServeHTTP(rec, req)
	return rec
}

// decodeAuthError reads the error code and message from a rejected response.
func decodeAuthError(t *testing.T, rec *httptest.ResponseRecorder) (code, message string) {
	t.Helper()
	var body struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	return body.Error, body.Message
}

func TestMiddlewareDistinguishes403Subtypes(t *testing.T) {
	remoteFailures := []struct {
		name        string
		allowRemote bool
		secretKey   string
		clientIP    string
		suppliedKey string
		wantStatus  int
		wantCode    string
	}{
		{
			name:        "remote management disabled",
			allowRemote: false,
			secretKey:   "$2a$10$abcdefghijklmnopqrstuv",
			clientIP:    "203.0.113.10",
			suppliedKey: "anything",
			wantStatus:  http.StatusForbidden,
			wantCode:    AuthReasonRemoteDisabled,
		},
		{
			name:        "management key not configured",
			allowRemote: true,
			secretKey:   "",
			clientIP:    "203.0.113.10",
			suppliedKey: "anything",
			wantStatus:  http.StatusForbidden,
			wantCode:    AuthReasonKeyNotConfigured,
		},
	}

	for _, tc := range remoteFailures {
		t.Run(tc.name, func(t *testing.T) {
			h := newAuthTestHandler(tc.allowRemote, tc.secretKey)
			rec := callMiddleware(t, h, tc.clientIP, tc.suppliedKey)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			code, message := decodeAuthError(t, rec)
			if code != tc.wantCode {
				t.Fatalf("error code = %q, want %q", code, tc.wantCode)
			}
			if message == "" {
				t.Fatal("expected a public message alongside the error code")
			}
		})
	}
}

func TestMiddlewareRejectsMissingAndInvalidKeysWith401(t *testing.T) {
	cases := []struct {
		name        string
		suppliedKey string
		wantCode    string
	}{
		{name: "missing key", suppliedKey: "", wantCode: AuthReasonMissingKey},
		{name: "invalid key", suppliedKey: "definitely-wrong", wantCode: AuthReasonInvalidKey},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A bcrypt hash of "correct-horse" so the comparison takes the real path.
			h := newAuthTestHandler(true, mustHashSecret(t, "correct-horse"))
			rec := callMiddleware(t, h, "127.0.0.1", tc.suppliedKey)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
			}
			code, _ := decodeAuthError(t, rec)
			if code != tc.wantCode {
				t.Fatalf("error code = %q, want %q", code, tc.wantCode)
			}
		})
	}
}

func TestMiddlewareRateLimitReportsRetryAfterAndMachineCode(t *testing.T) {
	h := newAuthTestHandler(true, mustHashSecret(t, "correct-horse"))

	// Drive the client to the ban threshold with wrong keys.
	for i := 0; i < 5; i++ {
		rec := callMiddleware(t, h, "198.51.100.7", "wrong")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d status = %d, want 401", i+1, rec.Code)
		}
	}

	rec := callMiddleware(t, h, "198.51.100.7", "correct-horse")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("banned attempt status = %d, want %d", rec.Code, http.StatusForbidden)
	}

	var body struct {
		Error      string `json:"error"`
		Message    string `json:"message"`
		RetryAfter int    `json:"retry_after"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode ban body %q: %v", rec.Body.String(), err)
	}
	if body.Error != AuthReasonRateLimited {
		t.Fatalf("error code = %q, want %q", body.Error, AuthReasonRateLimited)
	}
	if body.RetryAfter <= 0 || body.RetryAfter > 30*60 {
		t.Fatalf("retry_after = %d, want a positive value within the ban window", body.RetryAfter)
	}
}

func TestMiddlewareSuccessPathIsUnaffected(t *testing.T) {
	h := newAuthTestHandler(true, mustHashSecret(t, "correct-horse"))
	rec := callMiddleware(t, h, "127.0.0.1", "correct-horse")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("successful auth must not write an error body, got %q", rec.Body.String())
	}
}

func TestMiddlewareNeverLeaksInternalDetail(t *testing.T) {
	h := newAuthTestHandler(false, "")
	rec := callMiddleware(t, h, "203.0.113.10", "anything")

	body := rec.Body.String()
	for _, leak := range []string{
		"IP banned",
		"remote management key not set",
		"tryagain",
		"bcrypt",
		"203.0.113.10",
	} {
		if strings.Contains(body, leak) {
			t.Fatalf("response body leaks %q: %s", leak, body)
		}
	}
}

func TestAuthenticateManagementKeyKeepsLegacyMessagesForInternalCallers(t *testing.T) {
	// The Redis queue protocol prefix-matches these strings, so they must not drift.
	h := newAuthTestHandler(false, "")
	allowed, status, msg := h.AuthenticateManagementKey("203.0.113.10", false, "anything")
	if allowed || status != http.StatusForbidden || msg != "remote management disabled" {
		t.Fatalf("remote-disabled legacy result = (%v, %d, %q)", allowed, status, msg)
	}

	h = newAuthTestHandler(true, "")
	allowed, status, msg = h.AuthenticateManagementKey("203.0.113.10", false, "anything")
	if allowed || status != http.StatusForbidden || msg != "remote management key not set" {
		t.Fatalf("key-not-set legacy result = (%v, %d, %q)", allowed, status, msg)
	}

	h = newAuthTestHandler(true, mustHashSecret(t, "correct-horse"))
	allowed, status, msg = h.AuthenticateManagementKey("127.0.0.1", true, "")
	if allowed || status != http.StatusUnauthorized || msg != "missing management key" {
		t.Fatalf("missing-key legacy result = (%v, %d, %q)", allowed, status, msg)
	}

	allowed, status, msg = h.AuthenticateManagementKey("127.0.0.1", true, "wrong")
	if allowed || status != http.StatusUnauthorized || msg != "invalid management key" {
		t.Fatalf("invalid-key legacy result = (%v, %d, %q)", allowed, status, msg)
	}
}

func TestAuthReasonPublicMessagesAreDistinctAndNonTechnical(t *testing.T) {
	reasons := []string{
		AuthReasonMissingKey,
		AuthReasonInvalidKey,
		AuthReasonKeyNotConfigured,
		AuthReasonRemoteDisabled,
		AuthReasonRateLimited,
	}

	seen := make(map[string]string, len(reasons))
	for _, reason := range reasons {
		message := AuthFailure{Reason: reason}.PublicMessage()
		if message == "" {
			t.Fatalf("reason %q has an empty public message", reason)
		}
		forbidden := []string{"IP banned", "bcrypt", "provider", "<html", "nginx", "try again in 29m"}
		for _, term := range forbidden {
			if strings.Contains(strings.ToLower(message), strings.ToLower(term)) {
				t.Fatalf("public message for %q leaks %q: %q", reason, term, message)
			}
		}
		if prev, dup := seen[message]; dup {
			t.Fatalf("reasons %q and %q share the public message %q", prev, reason, message)
		}
		seen[message] = reason
	}
}

// mustHashSecret bcrypt-hashes a plaintext secret for test fixtures.
func mustHashSecret(t *testing.T, plaintext string) string {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(plaintext), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash secret: %v", err)
	}
	return string(hash)
}
