package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestStreamResumeWiredOnStreamingRoutes proves the resume middleware is
// attached to the streaming endpoints, so a resume attempt reaching the real
// router is handled by the gateway rather than the handler.
func TestStreamResumeWiredOnStreamingRoutes(t *testing.T) {
	server := newTestServer(t)
	if server.streamResume == nil {
		t.Fatal("server must own a stream resume registry")
	}

	routes := []string{"/v1/chat/completions", "/v1/completions", "/v1/messages", "/v1/responses"}
	for _, route := range routes {
		t.Run(route, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, route, nil)
			req.Header.Set("Authorization", "Bearer test-key")
			req.Header.Set("X-Stream-Id", "unknown-stream")
			req.Header.Set("Last-Event-ID", "5")
			rr := httptest.NewRecorder()
			server.engine.ServeHTTP(rr, req)

			if rr.Code != http.StatusGone {
				t.Fatalf("%s status = %d, want 410 (resume of unknown stream)", route, rr.Code)
			}
			if got := rr.Header().Get("X-Stream-Id"); got != "" {
				t.Fatalf("%s echoed X-Stream-Id = %q on a rejected resume", route, got)
			}
		})
	}
}

// TestStreamResumeNotOptedInLeavesRouteUnchanged proves the middleware does not
// alter ordinary requests: no stream id is issued and no session is retained.
func TestStreamResumeNotOptedInLeavesRouteUnchanged(t *testing.T) {
	server := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)

	if got := rr.Header().Get("X-Stream-Id"); got != "" {
		t.Fatalf("X-Stream-Id = %q on a request that did not opt in", got)
	}
	if got := server.streamResume.Len(); got != 0 {
		t.Fatalf("retained %d sessions for a request that did not opt in", got)
	}
}
