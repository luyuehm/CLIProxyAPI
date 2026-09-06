package contentfilter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestLicenseProbeValid drives the probe against a fake KEEPER status
// endpoint that reports a valid license.
func TestLicenseProbeValid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/license/status" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("X-CPA-Management-Key") != "secret-key" {
			t.Errorf("expected management key header, got %q", r.Header.Get("X-CPA-Management-Key"))
		}
		_ = json.NewEncoder(w).Encode(licenseStatusView{
			Enabled: true, Status: "valid", Mode: "block",
			Features: []string{"content-filter"},
		})
	}))
	defer srv.Close()

	p := NewLicenseProbe(srv.URL, "secret-key")
	ok := p.CheckOnce(context.Background())
	if !ok {
		t.Fatalf("expected valid verdict, got false (status=%+v)", p.Status())
	}
	if !p.Valid() {
		t.Fatal("Valid() should be true after a valid verdict")
	}
	probes, failures := p.Stats()
	if probes != 1 || failures != 0 {
		t.Fatalf("expected 1 probe 0 failures, got %d/%d", probes, failures)
	}
}

// TestLicenseProbeInvalid covers every non-active verdict: expired, revoked,
// invalid, device_mismatch, error. All must be treated as not-OK so CPA
// degrades instead of silently running enterprise features. (unlicensed free
// tier is covered separately as OK.)
func TestLicenseProbeInvalid(t *testing.T) {
	for _, status := range []string{"expired", "revoked", "invalid", "device_mismatch", "error"} {
		status := status
		t.Run(status, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(licenseStatusView{Enabled: true, Status: status, Mode: "block"})
			}))
			defer srv.Close()

			p := NewLicenseProbe(srv.URL, "key")
			if p.CheckOnce(context.Background()) {
				t.Fatalf("status %q should be invalid, got valid", status)
			}
			if p.Valid() {
				t.Fatalf("status %q should leave Valid() false", status)
			}
		})
	}
}

// TestLicenseProbeUnlicensedFreeTier asserts the free-tier status
// (enabled=false, status=unlicensed) stays OK for backward compatibility:
// a KEEPER with no license subsystem is not a license failure.
func TestLicenseProbeUnlicensedFreeTier(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(licenseStatusView{Enabled: false, Status: "unlicensed", Mode: "none"})
	}))
	defer srv.Close()

	p := NewLicenseProbe(srv.URL, "key")
	if !p.CheckOnce(context.Background()) {
		t.Fatalf("unlicensed free tier should be OK, got invalid (status=%+v)", p.Status())
	}
	if !p.Valid() {
		t.Fatal("Valid() should be true on unlicensed free tier")
	}
}

// TestLicenseProbeNon200 treats any non-2xx status response as a probe failure
// (e.g. 401 wrong management key) — the KEEPER-license-401 alert case.
func TestLicenseProbeNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"authentication required"}`))
	}))
	defer srv.Close()

	p := NewLicenseProbe(srv.URL, "wrong-key")
	if p.CheckOnce(context.Background()) {
		t.Fatal("expected probe to fail on 401")
	}
	if p.Valid() {
		t.Fatal("Valid() must be false after 401")
	}
}

// TestLicenseProbeTransitionLogs asserts the probe converges to a verdict and
// the Start/Stop lifecycle is safe.
func TestLicenseProbeTransitionAndLifecycle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(licenseStatusView{Enabled: true, Status: "valid", Mode: "block"})
	}))
	defer srv.Close()

	p := NewLicenseProbe(srv.URL, "key")
	p.interval = 20 * time.Millisecond
	p.Start()
	defer p.Stop()
	time.Sleep(80 * time.Millisecond)
	if !p.Valid() {
		t.Fatalf("expected valid after poll loop, status=%+v", p.Status())
	}
	if probes, _ := p.Stats(); probes < 2 {
		t.Fatalf("expected multiple probes from poll loop, got %d", probes)
	}
}

// TestLicenseProbeNil is a no-op safety net: nil probe reports open (true) so
// callers that never configure a probe keep pre-RIC-476 behavior.
func TestLicenseProbeNil(t *testing.T) {
	var p *LicenseProbe
	if !p.Valid() {
		t.Fatal("nil probe must report valid (open)")
	}
}
