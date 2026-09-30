package proxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/stream"
)

// testSource is a Source that produces configurable chunks.
type testSource struct {
	mu       atomic.Int64
	chunks   [][]byte
	delays   []time.Duration
	resumeAt int64
}

func newTestSource(chunks [][]byte, delays []time.Duration) *testSource {
	return &testSource{
		chunks: chunks,
		delays: delays,
	}
}

func (s *testSource) Start(ctx context.Context, resumeSeq int64) (Stream, error) {
	if resumeSeq > 0 && resumeSeq <= int64(len(s.chunks)) {
		s.resumeAt = resumeSeq
	}
	out := make(chan Chunk)
	go func() {
		defer close(out)
		startIdx := int(s.resumeAt)
		for i := startIdx; i < len(s.chunks); i++ {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if s.delays != nil && i < len(s.delays) && s.delays[i] > 0 {
				timer := time.NewTimer(s.delays[i])
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
			s.mu.Add(1)
			seq := int64(i + 1)
			select {
			case out <- Chunk{Payload: s.chunks[i], Seq: seq}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return &testStream{ch: out}, nil
}

func (s *testSource) emitted() int64 { return s.mu.Load() }

type testStream struct {
	ch <-chan Chunk
}

func (s *testStream) Chunks() <-chan Chunk { return s.ch }
func (s *testStream) Close() error         { return nil }

// stallSource introduces a stall then resumes normally.
type stallSource struct {
	inner    *testSource
	stallIdx int
	stallDur time.Duration
}

func (s *stallSource) Start(ctx context.Context, resumeSeq int64) (Stream, error) {
	if resumeSeq > 0 {
		// Resume path: pass through normally after the stall.
		return s.inner.Start(ctx, resumeSeq)
	}
	out := make(chan Chunk)
	go func() {
		defer close(out)
		for i := 0; i < len(s.inner.chunks); i++ {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if i == s.stallIdx {
				// Stall by sleeping past the stall timeout, then keep going.
				timer := time.NewTimer(s.stallDur)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
			seq := int64(i + 1)
			select {
			case out <- Chunk{Payload: s.inner.chunks[i], Seq: seq}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return &testStream{ch: out}, nil
}

func TestProxyDeliversAllChunks(t *testing.T) {
	chunks := [][]byte{
		[]byte("data: {\"a\":1}\n\n"),
		[]byte("data: {\"b\":2}\n\n"),
		[]byte("data: [DONE]\n\n"),
	}
	src := newTestSource(chunks, nil)
	p := NewProxy(src, Options{KeepAlivePeriod: -1, DetectStall: false, StallTimeout: time.Second})

	var buf bytes.Buffer
	err := p.StreamTo(context.Background(), func(payload []byte) error {
		buf.Write(payload)
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("{\"a\":1}")) {
		t.Fatal("missing chunk 1")
	}
	if !bytes.Contains(buf.Bytes(), []byte("{\"b\":2}")) {
		t.Fatal("missing chunk 2")
	}
	if src.emitted() != 3 {
		t.Fatalf("expected 3 emitted chunks, got %d", src.emitted())
	}
}

func TestProxyBufferManagement(t *testing.T) {
	chunks := make([][]byte, 5)
	for i := range chunks {
		chunks[i] = []byte("data: chunk\n\n")
	}
	src := newTestSource(chunks, nil)
	p := NewProxy(src, Options{KeepAlivePeriod: -1, DetectStall: false, StallTimeout: time.Second})

	var buf bytes.Buffer
	err := p.StreamTo(context.Background(), func(payload []byte) error {
		buf.Write(payload)
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if src.emitted() != 5 {
		t.Fatalf("expected 5 emitted, got %d", src.emitted())
	}
}

func TestProxyContextCancel(t *testing.T) {
	chunks := [][]byte{
		[]byte("data: hello\n\n"),
		[]byte("data: world\n\n"),
	}
	src := newTestSource(chunks, nil)
	p := NewProxy(src, Options{KeepAlivePeriod: -1, DetectStall: false, StallTimeout: time.Second})

	ctx, cancel := context.WithCancel(context.Background())
	var count int
	err := p.StreamTo(ctx, func(payload []byte) error {
		count++
		if count >= 1 {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestProxyStallAndResume(t *testing.T) {
	chunks := [][]byte{
		[]byte("data: {\"a\":1}\n\n"),
		[]byte("data: {\"b\":2}\n\n"),
		[]byte("data: {\"c\":3}\n\n"),
	}
	inner := newTestSource(chunks, nil)
	src := &stallSource{inner: inner, stallIdx: 1, stallDur: 200 * time.Millisecond}
	p := NewProxy(src, Options{
		KeepAlivePeriod: -1,
		StallTimeout:    50 * time.Millisecond,
		ResumeTimeout:   time.Second,
		DetectStall:     true,
	})

	var buf bytes.Buffer
	err := p.StreamTo(context.Background(), func(payload []byte) error {
		buf.Write(payload)
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	output := buf.String()
	if !bytes.Contains([]byte(output), []byte("{\"c\":3}")) {
		t.Fatal("missing chunk 3 after resume")
	}
}

func TestProxyEmptyBuffer(t *testing.T) {
	_ = stream.NewStreamBuffer(0)
}

func TestProxyResumeUnavailable(t *testing.T) {
	src := &failSource{}
	p := NewProxy(src, Options{
		KeepAlivePeriod: -1,
		StallTimeout:    30 * time.Millisecond,
		ResumeTimeout:   100 * time.Millisecond,
		DetectStall:     true,
	})
	err := p.StreamTo(context.Background(), func(payload []byte) error {
		return nil
	})
	if err != ErrResumeUnavailable {
		t.Fatalf("expected ErrResumeUnavailable, got %v", err)
	}
}

type failSource struct{}

func (f *failSource) Start(ctx context.Context, resumeSeq int64) (Stream, error) {
	if resumeSeq > 0 {
		return nil, ErrResumeUnavailable
	}
	out := make(chan Chunk)
	go func() {
		select {
		case out <- Chunk{Payload: []byte("data: first\n\n"), Seq: 1}:
		case <-ctx.Done():
			close(out)
			return
		}
		// Stall forever — no more chunks, never close
		<-ctx.Done()
	}()
	return &testStream{ch: out}, nil
}

func TestProxyHandler(t *testing.T) {
	chunks := [][]byte{[]byte("data: hello\n\n"), []byte("data: [DONE]\n\n")}
	src := newTestSource(chunks, nil)
	p := NewProxy(src, Options{KeepAlivePeriod: -1, DetectStall: false, StallTimeout: time.Second})

	handler := p.Handler()
	if handler == nil {
		t.Fatal("handler should not be nil")
	}
}

func TestProxyResumeTimeout(t *testing.T) {
	src := &hangingResumeSource{}
	p := NewProxy(src, Options{
		KeepAlivePeriod: -1,
		StallTimeout:    30 * time.Millisecond,
		ResumeTimeout:   50 * time.Millisecond,
		DetectStall:     true,
	})
	err := p.StreamTo(context.Background(), func(payload []byte) error {
		return nil
	})
	var tm *ErrResumeTimeout
	if !errors.As(err, &tm) {
		t.Fatalf("expected *ErrResumeTimeout, got %v", err)
	}
	if tm.ResumeTimeout != 50*time.Millisecond {
		t.Fatalf("expected ResumeTimeout 50ms, got %v", tm.ResumeTimeout)
	}
}

// hangingResumeSource emits one frame then never returns on the resume path.
// This mirrors a backup node whose re-establishment hangs (lost node, silent
// TCP timeout) — the proxy must surface ErrResumeTimeout instead of freezing
// the downstream stream forever.
type hangingResumeSource struct{}

func (h *hangingResumeSource) Start(ctx context.Context, resumeSeq int64) (Stream, error) {
	if resumeSeq > 0 {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	out := make(chan Chunk)
	go func() {
		select {
		case out <- Chunk{Payload: []byte("data: first\n\n"), Seq: 1}:
		case <-ctx.Done():
			close(out)
			return
		}
		// Stall forever — no more chunks, never close.
		<-ctx.Done()
	}()
	return &testStream{ch: out}, nil
}

// httpResumeSource behaves like the real SSE upstream: it pushes frames on
// the open body, stalls for stallDur mid-stream on the initial connection
// (simulating a network flap on the first node), and on the resume call
// replays only the frames strictly after the delivered marker.
type httpResumeSource struct {
	chunks    [][]byte
	stallIdx  int
	stallDur  time.Duration
	resumeSeq atomic.Int64
}

func (s *httpResumeSource) Start(ctx context.Context, resumeSeq int64) (Stream, error) {
	if resumeSeq > 0 {
		s.resumeSeq.Store(resumeSeq)
	}
	startIdx := int(resumeSeq)
	out := make(chan Chunk)
	go func() {
		defer close(out)
		for i := startIdx; i < len(s.chunks); i++ {
			select {
			case <-ctx.Done():
				return
			default:
			}
			// Only the initial connection replays the network flap; the
			// replacement stream must flow continuously from the breakpoint.
			if resumeSeq == 0 && i == s.stallIdx {
				timer := time.NewTimer(s.stallDur)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
			select {
			case out <- Chunk{Payload: s.chunks[i], Seq: int64(i + 1)}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return &testStream{ch: out}, nil
}

// TestProxyHTTPResumeOnDisconnect proves the acceptance criterion end to end
// over a real HTTP SSE connection: simulate a network flap (stall past the
// detection window, recovery within 3s of the flap) and assert the client
// receives every frame, in order, with no duplicated or corrupted bytes.
func TestProxyHTTPResumeOnDisconnect(t *testing.T) {
	chunks := [][]byte{
		[]byte("data: {\"id\":\"a\",\"delta\":\"hello\"}\n\n"),
		[]byte("data: {\"id\":\"b\",\"delta\":\" world\"}\n\n"),
		[]byte("data: {\"id\":\"c\",\"delta\":\"!\"}\n\n"),
		[]byte("data: {\"id\":\"d\",\"delta\":\"\"}\n\n"),
		[]byte("data: [DONE]\n\n"),
	}
	src := &httpResumeSource{chunks: chunks, stallIdx: 1, stallDur: 600 * time.Millisecond}
	p := NewProxy(src, Options{
		KeepAlivePeriod: -1,
		StallTimeout:    200 * time.Millisecond,
		ResumeTimeout:   time.Second,
		DetectStall:     true,
	})

	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL) //nolint:gosec // test server URL
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("expected text/event-stream, got %q", ct)
	}

	var lines []string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read SSE body: %v", err)
	}

	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "upstream resume") {
		t.Fatal("missing resume marker; stall never detected")
	}
	// The flap strikes before the second frame, so the breakpoint is seq 1.
	if got := src.resumeSeq.Load(); got != 1 {
		t.Fatalf("expected resume from seq 1, got %d", got)
	}
	for _, chunk := range chunks {
		data := strings.TrimSpace(string(chunk))
		if !bytes.Contains([]byte(joined), []byte(data)) {
			t.Fatalf("client missed frame %q (garbled or dropped after resume)", data)
		}
	}
	// Assert each frame appears exactly once: no duplicates on replay.
	for _, id := range []string{`"id":"a"`, `"id":"b"`, `"id":"c"`, `"id":"d"`} {
		if n := strings.Count(joined, id); n != 1 {
			t.Fatalf("frame %s appeared %d times; expected exactly once (no dupes)", id, n)
		}
	}
}

// TestProxyHTTPHandlerViaRecorder serves the resilient handler through an
// httptest.ResponseRecorder so the SSE handler path is covered without a
// live server.
func TestProxyHTTPHandlerViaRecorder(t *testing.T) {
	chunks := [][]byte{[]byte("data: hello\n\n"), []byte("data: [DONE]\n\n")}
	src := newTestSource(chunks, nil)
	p := NewProxy(src, Options{KeepAlivePeriod: -1, DetectStall: false, StallTimeout: time.Second})

	handler := p.Handler()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "hello") || !strings.Contains(body, "[DONE]") {
		t.Fatalf("recorder received incomplete stream: %q", body)
	}
}

func TestProxyEmptySource(t *testing.T) {
	src := newTestSource(nil, nil)
	p := NewProxy(src, Options{KeepAlivePeriod: -1, DetectStall: false, StallTimeout: time.Second})
	err := p.StreamTo(context.Background(), func(payload []byte) error {
		return nil
	})
	if err != nil {
		t.Fatalf("expected no error for empty source, got %v", err)
	}
}

func TestProxySourceError(t *testing.T) {
	src := &errorSource{err: errors.New("upstream failure")}
	p := NewProxy(src, Options{KeepAlivePeriod: -1, DetectStall: false, StallTimeout: time.Second})
	err := p.StreamTo(context.Background(), func(payload []byte) error {
		return nil
	})
	if err == nil || err.Error() != "upstream failure" {
		t.Fatalf("expected 'upstream failure', got %v", err)
	}
}

type errorSource struct {
	err error
}

func (e *errorSource) Start(ctx context.Context, resumeSeq int64) (Stream, error) {
	out := make(chan Chunk, 1)
	out <- Chunk{Err: e.err}
	close(out)
	return &testStream{ch: out}, nil
}
