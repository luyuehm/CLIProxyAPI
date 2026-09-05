package auth

import (
	"encoding/base32"
	"strings"
	"testing"
	"time"
)

func TestGenerateTOTPSecretIsValidBase32(t *testing.T) {
	secret, err := GenerateTOTPSecret()
	if err != nil {
		t.Fatalf("GenerateTOTPSecret returned error: %v", err)
	}
	if secret == "" {
		t.Fatal("expected non-empty TOTP secret")
	}
	decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil {
		t.Fatalf("generated secret is not valid unpadded base32: %v", err)
	}
	if len(decoded) != TOTPSecretSizeBytes {
		t.Fatalf("expected %d bytes of entropy, got %d", TOTPSecretSizeBytes, len(decoded))
	}
}

func TestBuildTOTPURIEncodesLabelAndIssuer(t *testing.T) {
	uri := BuildTOTPURI("LicenseAdmin", "admin", "JBSWY3DPEHPK3PXP")
	if !strings.HasPrefix(uri, "otpauth://totp/") {
		t.Fatalf("unexpected otpauth scheme: %q", uri)
	}
	if !strings.Contains(uri, "secret=JBSWY3DPEHPK3PXP") {
		t.Fatalf("expected secret in query: %q", uri)
	}
	if !strings.Contains(uri, "issuer=LicenseAdmin") {
		t.Fatalf("expected issuer in query: %q", uri)
	}
	if !strings.Contains(uri, "digits=6&period=30") {
		t.Fatalf("expected RFC 6238 defaults in query: %q", uri)
	}
	// 空格会被 percent-encoded，避免 Authenticator 解析歧义。
	if strings.Contains(uri, " ") {
		t.Fatalf("otpauth URI must not contain raw spaces: %q", uri)
	}
}

func TestTOTPCodeMatchesRFC6238Vector(t *testing.T) {
	// RFC 6238 附录 B：ASCII "12345678901234567890"（对应 Base32 GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ）。
	// 计数器 59 的 SHA-1 TOTP 验证码是 8 位 94287082；6 位截断形式为 287082。
	secret := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	at := time.Unix(59, 0)
	code, err := TOTPCodeAt(secret, at)
	if err != nil {
		t.Fatalf("TOTPCodeAt returned error: %v", err)
	}
	if code != "287082" {
		t.Fatalf("expected RFC 6238 6-digit vector 287082, got %q", code)
	}
}

func TestValidateTOTPCodeAcceptsCurrentAndSkewWindow(t *testing.T) {
	secret, err := GenerateTOTPSecret()
	if err != nil {
		t.Fatalf("GenerateTOTPSecret returned error: %v", err)
	}
	now := time.Now()

	current, err := TOTPCodeAt(secret, now)
	if err != nil {
		t.Fatalf("TOTPCodeAt returned error: %v", err)
	}
	if !ValidateTOTPCode(secret, current, now) {
		t.Fatal("expected current code to validate")
	}

	previous, err := TOTPCodeAt(secret, now.Add(-totpPeriodSeconds*time.Second))
	if err != nil {
		t.Fatalf("TOTPCodeAt returned error: %v", err)
	}
	if !ValidateTOTPCode(secret, previous, now) {
		t.Fatal("expected previous-period code to validate within skew window")
	}
}

func TestValidateTOTPCodeRejectsWrongAndStaleCodes(t *testing.T) {
	secret, err := GenerateTOTPSecret()
	if err != nil {
		t.Fatalf("GenerateTOTPSecret returned error: %v", err)
	}
	now := time.Now()

	if ValidateTOTPCode(secret, "000000", now) {
		t.Fatal("expected wrong code to be rejected")
	}
	stale, err := TOTPCodeAt(secret, now.Add(-3*totpPeriodSeconds*time.Second))
	if err != nil {
		t.Fatalf("TOTPCodeAt returned error: %v", err)
	}
	if ValidateTOTPCode(secret, stale, now) {
		t.Fatal("expected stale code outside skew window to be rejected")
	}
	if ValidateTOTPCode(secret, "abc123", now) {
		t.Fatal("expected non-numeric code to be rejected")
	}
}

func TestValidateTOTPCodeToleratesSpacesAndDashes(t *testing.T) {
	secret, err := GenerateTOTPSecret()
	if err != nil {
		t.Fatalf("GenerateTOTPSecret returned error: %v", err)
	}
	code, err := TOTPCodeAt(secret, time.Now())
	if err != nil {
		t.Fatalf("TOTPCodeAt returned error: %v", err)
	}
	normalized := NormalizeTOTPCode("  " + code[:3] + "-" + code[3:] + " ")
	if normalized != code {
		t.Fatalf("expected normalized code %q, got %q", code, normalized)
	}
	if !ValidateTOTPCode(secret, normalized, time.Now()) {
		t.Fatal("expected normalized code to validate")
	}
}
