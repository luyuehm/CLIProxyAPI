package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gin "github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/license"
)

func TestLicenseDegradedAllowedPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/management/settings", true},
		{"/api/v1/license", true},
		{"/api/anything", true},
		{"/", true},
		{"/healthz", true},
		{"/enterprise/", true},
		{"/v1/chat/completions", false},
		{"/v1/models", false},
		{"/v1", false},
		{"/some/other/path", false},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			if got := licenseDegradedAllowedPath(tc.path); got != tc.want {
				t.Fatalf("licenseDegradedAllowedPath(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

func newLicenseMiddlewareTestRouter(s *Server) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(s.licenseDegradedMiddleware())
	r.NoRoute(func(c *gin.Context) { c.Status(http.StatusOK) })
	return r
}

func TestLicenseDegradedMiddleware_NotDegradedPassesThrough(t *testing.T) {
	t.Cleanup(func() { license.SetDegraded(false) })
	license.SetDegraded(false)

	r := newLicenseMiddlewareTestRouter(&Server{})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 when not degraded, got %d", rec.Code)
	}
	if h := rec.Header().Get("X-License-Status"); h != "" {
		t.Fatalf("expected no X-License-Status header when not degraded, got %q", h)
	}
}

func TestLicenseDegradedMiddleware_DegradedBlocksProxy(t *testing.T) {
	t.Cleanup(func() { license.SetDegraded(false) })
	license.SetDegraded(true)

	r := newLicenseMiddlewareTestRouter(&Server{})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 when degraded on proxy path, got %d", rec.Code)
	}
	if h := rec.Header().Get("X-License-Status"); h != "degraded" {
		t.Fatalf("expected X-License-Status=degraded, got %q", h)
	}
	if got := rec.Body.String(); !strings.Contains(got, "license_degraded") {
		t.Fatalf("expected body to contain license_degraded, got %s", got)
	}
}

func TestLicenseDegradedMiddleware_DegradedAllowsManagementAndHealth(t *testing.T) {
	t.Cleanup(func() { license.SetDegraded(false) })
	license.SetDegraded(true)

	r := newLicenseMiddlewareTestRouter(&Server{})
	for _, p := range []string{"/healthz", "/management/settings", "/api/v1/license", "/enterprise/"} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 for allowed path %q when degraded, got %d", p, rec.Code)
		}
		if h := rec.Header().Get("X-License-Status"); h != "" {
			t.Fatalf("expected no X-License-Status header for allowed path %q, got %q", p, h)
		}
	}
}
