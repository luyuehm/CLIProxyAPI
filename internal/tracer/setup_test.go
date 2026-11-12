package tracer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestMiddlewareAdoptsValidTraceparent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(Middleware())
	r.GET("/", func(c *gin.Context) {
		trace := TraceContextFromContext(c.Request.Context())
		if trace == nil || !trace.Adopted {
			c.AbortWithStatus(http.StatusNotAcceptable)
			return
		}
		c.String(http.StatusOK, trace.TraceID)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(traceparentHeader, "00-0af7651912cd7359c451f5b756e546e8-c451f5b756e546e7-01")
	req.Header.Set(tracestateHeader, "vendor=abc")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "0af7651912cd7359c451f5b756e546e8" {
		t.Fatalf("adopted trace id not in context: %q", rec.Body.String())
	}
	// The response must echo the trace context back to the client.
	if got := rec.Header().Get(traceparentHeader); got == "" {
		t.Fatal("response must echo a traceparent header")
	}
	if got := rec.Header().Get(tracestateHeader); got != "vendor=abc" {
		t.Fatalf("tracestate = %q, want passthrough vendor=abc", got)
	}
}

func TestMiddlewareGeneratesWhenAbsent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(Middleware())
	var seeded bool
	r.GET("/", func(c *gin.Context) {
		seeded = TraceContextFromContext(c.Request.Context()) != nil
		c.String(http.StatusOK, "")
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if !seeded {
		t.Fatal("request with no incoming traceparent must still get a generated trace context")
	}
}

func TestMiddlewareRejectsInvalidTraceparent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(Middleware())
	var adopted bool
	r.GET("/", func(c *gin.Context) {
		trace := TraceContextFromContext(c.Request.Context())
		adopted = trace != nil && trace.Adopted
		c.String(http.StatusOK, "")
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	// Wrong segment width: must be rejected, not adopted.
	req.Header.Set(traceparentHeader, "00-short-c451f5b756e546e7-01")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if adopted {
		t.Fatal("invalid traceparent must not be adopted")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a rejected context still yields a generated trace)", rec.Code)
	}
}

func TestAdoptFromRequestNil(t *testing.T) {
	trace := AdoptFromRequest(nil)
	if trace == nil || trace.TraceID == "" {
		t.Fatal("nil request must yield a generated trace")
	}
}

func TestBuildDisabledByDefault(t *testing.T) {
	t.Setenv("CPA_TRACE_ENABLED", "")
	t.Setenv("LANGSMITH_TRACING", "")
	if runtime, active := Build(); active || runtime != nil {
		t.Fatalf("unconfigured Build() must be inactive, got active=%v runtime=%v", active, runtime)
	}
}

func TestBuildRequiresKeeperSource(t *testing.T) {
	t.Setenv("CPA_TRACE_ENABLED", "true")
	t.Setenv("CPA_TRACE_KEEPER_URL", "")
	t.Setenv("CPA_BASE_URL", "")
	if runtime, active := Build(); active {
		t.Fatalf("enabled but sourceless Build() must be inactive, got runtime=%v", runtime)
	}
}

func TestBuildFromEnv(t *testing.T) {
	t.Setenv("CPA_TRACE_ENABLED", "true")
	t.Setenv("CPA_TRACE_KEEPER_URL", "http://keeper:4320")
	t.Setenv("CPA_TRACE_KEEPER_KEY", "mgmt-key")
	t.Setenv("CPA_TRACE_INGEST_TOKEN", "")
	runtime, active := Build()
	if !active || runtime == nil {
		t.Fatal("configured Build() must be active")
	}
	if runtime.Reporter() == nil {
		t.Fatal("runtime must expose its reporter")
	}
	runtime.Start()
	runtime.Stop()
	// Neutralize the named plugin slot on the process-wide default usage
	// manager so no other test in this package triggers trace reports.
	coreusage.RegisterNamedPlugin("tracer", noopUsagePlugin{})
}

// noopUsagePlugin implements coreusage.Plugin as a no-op, used to clear
// the tracer's named slot after a Build() test.
type noopUsagePlugin struct{}

func (noopUsagePlugin) HandleUsage(_ context.Context, _ coreusage.Record) {}

func TestBuildLangsmithAlias(t *testing.T) {
	t.Setenv("CPA_TRACE_ENABLED", "")
	t.Setenv("LANGSMITH_TRACING", "true")
	t.Setenv("CPA_TRACE_KEEPER_URL", "http://keeper:4320")
	t.Setenv("CPA_TRACE_KEEPER_KEY", "mgmt-key")
	runtime, active := Build()
	if !active {
		t.Fatal("LANGSMITH_TRACING=true must enable the channel")
	}
	runtime.Reporter().Stop()
	coreusage.RegisterNamedPlugin("tracer", noopUsagePlugin{})
}

func TestBuildDisabledExplicit(t *testing.T) {
	t.Setenv("CPA_TRACE_ENABLED", "false")
	t.Setenv("LANGSMITH_TRACING", "true")
	t.Setenv("CPA_TRACE_KEEPER_URL", "http://keeper:4320")
	if _, active := Build(); active {
		t.Fatal("explicit CPA_TRACE_ENABLED=false must win over the alias")
	}
}
