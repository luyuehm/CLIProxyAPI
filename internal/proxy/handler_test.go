package proxy

import (
	"bytes"
	"context"
	"errors"
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
