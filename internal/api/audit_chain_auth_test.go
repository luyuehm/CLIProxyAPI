package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestAuditChainRoutesRequireManagementAuth pins the acceptance scenario 2
// guarantee at the routing layer: the RIC-2686 audit endpoints are registered
// inside the authenticated /v0/management group, so a caller without the
// management key is answered 401 before the injected handler runs and no
// record content can leak. The stub handler returns a sentinel body so a
// test failure that reaches the handler is unambiguous.
func TestAuditChainRoutesRequireManagementAuth(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-key")

	const sentinel = `{"audit":"reached-handler"}`
	server := newTestServerWithOptions(t, WithAuditChainHandler(func(c *gin.Context) {
		c.String(http.StatusOK, sentinel)
	}))

	for _, target := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/v0/management/audit/operations/op-a"},
		{http.MethodPost, "/v0/management/audit/events"},
	} {
		missing := httptest.NewRequest(target.method, target.path, nil)
		missingRR := httptest.NewRecorder()
		server.engine.ServeHTTP(missingRR, missing)
		if missingRR.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without key = %d, want 401; body=%s", target.method, target.path, missingRR.Code, missingRR.Body.String())
		}
		if strings.Contains(missingRR.Body.String(), sentinel) {
			t.Fatalf("%s %s leaked handler content without auth", target.method, target.path)
		}

		wrong := httptest.NewRequest(target.method, target.path, nil)
		wrong.Header.Set("Authorization", "Bearer wrong-key")
		wrongRR := httptest.NewRecorder()
		server.engine.ServeHTTP(wrongRR, wrong)
		if wrongRR.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s with wrong key = %d, want 401", target.method, target.path, wrongRR.Code)
		}
	}

	// With the key the injected handler runs — proving the route is live and
	// only gated on auth, not broken.
	authed := httptest.NewRequest(http.MethodGet, "/v0/management/audit/operations/op-a", nil)
	authed.Header.Set("Authorization", "Bearer test-management-key")
	authedRR := httptest.NewRecorder()
	server.engine.ServeHTTP(authedRR, authed)
	if authedRR.Code != http.StatusOK || !strings.Contains(authedRR.Body.String(), "reached-handler") {
		t.Fatalf("authenticated audit route = %d, want 200 + handler body; got %s", authedRR.Code, authedRR.Body.String())
	}
}

// TestAuditChainRoutesReport404WhenChannelOff pins the "feature off" path:
// without an injected handler the audit routes report 404 rather than
// guessing or serving an empty body.
func TestAuditChainRoutesReport404WhenChannelOff(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-key")
	// No WithAuditChainHandler: the channel is off.
	server := newTestServer(t)

	for _, target := range []string{
		"/v0/management/audit/operations/op-a",
		"/v0/management/audit/events",
	} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.Header.Set("Authorization", "Bearer test-management-key")
		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("%s channel-off = %d, want 404", target, rr.Code)
		}
	}
}
