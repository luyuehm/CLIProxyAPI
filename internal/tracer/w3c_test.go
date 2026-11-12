package tracer

import (
	"context"
	"strings"
	"testing"

	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestParseTraceparent(t *testing.T) {
	const validTrace = "0af7651912cd7359c451f5b756e546e8"
	cases := []struct {
		name    string
		raw     string
		wantErr bool
		traceID string
		spanID  string
		sampled bool
	}{
		{
			name:    "non-hex segment rejected",
			raw:     "00-0af7651912cd7359-00511f7b756e5v5e-01",
			wantErr: true,
		},
		{
			name:    "valid sampled",
			raw:     "00-" + validTrace + "-c451f5b756e546e7-01",
			traceID: validTrace,
			spanID:  "c451f5b756e546e7",
			sampled: true,
		},
		{
			name:    "valid not sampled",
			raw:     "00-" + validTrace + "-c451f5b756e546e7-00",
			traceID: validTrace,
			spanID:  "c451f5b756e546e7",
		},
		{
			name:    "empty",
			raw:     "",
			wantErr: true,
		},
		{
			name:    "wrong version",
			raw:     "01-" + validTrace + "-c451f5b756e546e7-01",
			wantErr: true,
		},
		{
			name:    "all zero trace id",
			raw:     "00-00000000000000000000000000000000-c451f5b756e546e7-01",
			wantErr: true,
		},
		{
			name:    "all zero parent id",
			raw:     "00-" + validTrace + "-0000000000000000-01",
			wantErr: true,
		},
		{
			name:    "bad segment count",
			raw:     "00-" + validTrace + "-c451f5b756e546e7",
			wantErr: true,
		},
		{
			name:    "short trace id width",
			raw:     "00-0af7651912cd7359-c451f5b756e546e7-01",
			wantErr: true,
		},
		{
			name:    "bad segment width",
			raw:     "00-" + validTrace + "-c451f5b756e546e-01",
			wantErr: true,
		},
		{
			name:    "non-hex flags",
			raw:     "00-" + validTrace + "-c451f5b756e546e7-XX",
			wantErr: true,
		},
		{
			name:    "trims whitespace",
			raw:     " 00-" + validTrace + "-c451f5b756e546e7-01 ",
			traceID: validTrace,
			spanID:  "c451f5b756e546e7",
			sampled: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseTraceparent(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseTraceparent(%q) = %+v, want error", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseTraceparent(%q) error: %v", tc.raw, err)
			}
			if got.TraceID != tc.traceID {
				t.Fatalf("TraceID = %q, want %q", got.TraceID, tc.traceID)
			}
			if got.ParentSpanID != tc.spanID {
				t.Fatalf("ParentSpanID = %q, want %q", got.ParentSpanID, tc.spanID)
			}
			if got.Sampled != tc.sampled {
				t.Fatalf("Sampled = %v, want %v", got.Sampled, tc.sampled)
			}
			if !got.Adopted {
				t.Fatalf("Adopted = false, want true")
			}
		})
	}
}

func TestGenerateTraceContext(t *testing.T) {
	trace := GenerateTraceContext()
	if len(trace.TraceID) != traceIDHexLen {
		t.Fatalf("TraceID width = %d, want %d", len(trace.TraceID), traceIDHexLen)
	}
	if !trace.Sampled {
		t.Fatalf("Sampled = false, want true")
	}
	if trace.Adopted {
		t.Fatalf("Adopted = true, want false")
	}
	other := GenerateTraceContext()
	if trace.TraceID == other.TraceID {
		t.Fatalf("two generated traces share TraceID %q", trace.TraceID)
	}
}

func TestFormatTraceparent(t *testing.T) {
	trace := &TraceContext{TraceID: "0af7651912cd7359c451f5b756e546e8", ParentSpanID: "c451f5b756e546e7", Sampled: true}
	// An explicit span id round-trips into the parent slot of the header.
	if got := FormatTraceparent(trace, "c451f5b756e546e7"); got != "00-0af7651912cd7359c451f5b756e546e8-c451f5b756e546e7-01" {
		t.Fatalf("FormatTraceparent explicit = %q", got)
	}
	// A non-sampled trace renders flags 00.
	if got := FormatTraceparent(&TraceContext{TraceID: trace.TraceID}, "c451f5b756e546e7"); got != "00-0af7651912cd7359c451f5b756e546e8-c451f5b756e546e7-00" {
		t.Fatalf("FormatTraceparent not-sampled = %q", got)
	}
	// No context: the header is omitted entirely.
	if got := FormatTraceparent(nil, "c451f5b756e546e7"); got != "" {
		t.Fatalf("FormatTraceparent(nil) = %q, want empty", got)
	}
	// An omitted span id still produces a parseable header.
	generated := FormatTraceparent(trace, "")
	if generated == "" {
		t.Fatal("empty span id must still produce a header")
	}
	if _, err := ParseTraceparent(generated); err != nil {
		t.Fatalf("generated header not parseable: %v (%q)", err, generated)
	}
}

func TestDeterministicSpanID(t *testing.T) {
	a := DeterministicSpanID("trace-1", "req-1")
	b := DeterministicSpanID("trace-1", "req-1")
	if a != b {
		t.Fatalf("DeterministicSpanID not deterministic: %q vs %q", a, b)
	}
	if len(a) != spanIDHexLen {
		t.Fatalf("span id width = %d, want %d", len(a), spanIDHexLen)
	}
	if DeterministicSpanID("trace-1", "req-2") == a {
		t.Fatalf("different request id must yield a different span id")
	}
	if DeterministicSpanID("trace-2", "req-1") == a {
		t.Fatalf("different trace id must yield a different span id")
	}
	if DeterministicSpanID("", "") == "" {
		t.Fatalf("empty inputs must still yield an id")
	}
}

func TestWithTraceContext(t *testing.T) {
	trace := GenerateTraceContext()
	ctx := WithTraceContext(context.Background(), trace)
	got := TraceContextFromContext(ctx)
	if got != trace {
		t.Fatalf("TraceContextFromContext = %p, want %p", got, trace)
	}
	if TraceContextFromContext(context.Background()) != nil {
		t.Fatalf("plain context must carry no trace")
	}
	if TraceContextFromContext(nil) != nil {
		t.Fatalf("nil context must return nil")
	}
}

// TestBuildIngestRequest covers the record → payload conversion, including
// adoption vs generation, the degenerate-record guard, and the sensitive
// attribute-key rule (no attribute key may contain a KEEPER-side
// sensitive word).
func TestBuildIngestRequest(t *testing.T) {
	base := coreusage.Record{
		Model:     "gpt-test",
		Provider:  "openai",
		SessionID: "sess-1",
		Detail:    coreusage.Detail{InputTokens: 10, OutputTokens: 20, TotalTokens: 30, CacheReadTokens: 4},
	}

	t.Run("adopts trace from context", func(t *testing.T) {
		const adoptedTraceID = "0af7651912cd7359c451f5b756e546e8"
		trace, err := ParseTraceparent("00-" + adoptedTraceID + "-c451f5b756e546e7-01")
		if err != nil {
			t.Fatal(err)
		}
		ctx := WithTraceContext(internallogging.WithRequestID(context.Background(), "req-1"), trace)
		payload, ok := BuildIngestRequest(ctx, base)
		if !ok {
			t.Fatal("expected ok")
		}
		if payload.TraceID != adoptedTraceID {
			t.Fatalf("TraceID = %q, want adopted id", payload.TraceID)
		}
		if payload.RequestID != "req-1" {
			t.Fatalf("RequestID = %q, want req-1", payload.RequestID)
		}
		if payload.SessionID != "sess-1" {
			t.Fatalf("SessionID = %q, want sess-1", payload.SessionID)
		}
		span := payload.Spans[0]
		if span.Kind != "llm" || span.Status != "ok" {
			t.Fatalf("span = %+v, want kind=llm status=ok", span)
		}
		if span.InputTokens != 10 || span.OutputTokens != 20 || span.CacheReadTokens != 4 {
			t.Fatalf("token fields = %+v", span)
		}
		if span.ParentSpanID != "c451f5b756e546e7" {
			t.Fatalf("ParentSpanID = %q, want adopted parent", span.ParentSpanID)
		}
		if payload.Traceparent == "" {
			t.Fatal("adopted context must carry the traceparent carrier")
		}
	})

	t.Run("generates trace when absent", func(t *testing.T) {
		ctx := internallogging.WithRequestID(context.Background(), "req-2")
		payload, ok := BuildIngestRequest(ctx, base)
		if !ok {
			t.Fatal("expected ok")
		}
		if payload.TraceID == "" || len(payload.TraceID) != traceIDHexLen {
			t.Fatalf("generated TraceID = %q", payload.TraceID)
		}
		if payload.Traceparent != "" {
			t.Fatalf("self-generated context must not carry a traceparent (no upstream parent)")
		}
		if payload.Spans[0].ParentSpanID != "" {
			t.Fatalf("self-generated span must have no parent")
		}
	})

	t.Run("failed record marks error span", func(t *testing.T) {
		failed := base
		failed.Failed = true
		failed.Fail = coreusage.Failure{StatusCode: 429, Body: "rate limited"}
		payload, ok := BuildIngestRequest(context.Background(), failed)
		if !ok {
			t.Fatal("expected ok")
		}
		if payload.Spans[0].Status != "error" {
			t.Fatalf("Status = %q, want error", payload.Spans[0].Status)
		}
		if payload.Spans[0].ErrorMessage == "" {
			t.Fatal("failed span must carry an error message")
		}
	})

	t.Run("degenerate record is skipped", func(t *testing.T) {
		if _, ok := BuildIngestRequest(context.Background(), coreusage.Record{}); ok {
			t.Fatal("record with no model must not be reported")
		}
	})

	t.Run("attribute keys survive KEEPER sensitive filter", func(t *testing.T) {
		payload, _ := BuildIngestRequest(context.Background(), base)
		for key := range payload.Spans[0].Attributes {
			for _, sensitive := range []string{"authorization", "password", "secret", "apikey", "credential", "cookie", "bearer", "signature"} {
				if strings.Contains(strings.ToLower(key), sensitive) {
					t.Fatalf("attribute key %q contains sensitive word %q", key, sensitive)
				}
			}
		}
	})

	t.Run("error message is capped", func(t *testing.T) {
		long := coreusage.Record{Model: "m", Failed: true, Fail: coreusage.Failure{Body: strings.Repeat("x", 2000)}}
		payload, _ := BuildIngestRequest(context.Background(), long)
		if len(payload.Spans[0].ErrorMessage) > maxErrorMessage {
			t.Fatalf("error message length = %d, want <= %d", len(payload.Spans[0].ErrorMessage), maxErrorMessage)
		}
	})
}
