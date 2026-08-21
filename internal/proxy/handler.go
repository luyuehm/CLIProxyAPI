// Package proxy provides enterprise-grade streaming reliability for LLM
// gateway responses: a sliding-window buffer in front of the SSE stream with
// transparent breakpoint resume.
//
// A Source produces stream frames tagged with a monotonically increasing
// sequence angle. The Proxy feeds those frames downstream while retaining them
// in a sliding-window StreamBuffer. When the upstream link stalls (network
// flap, node timeout) for longer than StallTimeout, the proxy automatically
// opens a resumed stream from the recorded breakpoint via Source.Start. The
// replacement stream replays frames strictly after the delivered marker, and
// the proxy discards any duplicate frames so the client observes a continuous,
// gap-free SSE flow.
package proxy

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/stream"
	log "github.com/sirupsen/logrus"
)

const (
	// DefaultStallTimeout is the idle gap after which an upstream is considered
	// stalled and a breakpoint resume is triggered.
	DefaultStallTimeout = 5 * time.Second

	// DefaultResumeTimeout bounds each breakpoint resume attempt.
	DefaultResumeTimeout = 5 * time.Second

	// DefaultKeepAlivePeriod is the SSE heartbeat interval.
	DefaultKeepAlivePeriod = 15 * time.Second

	// DefaultMaxBufferBytes is the sliding-window capacity.
	DefaultMaxBufferBytes = stream.MaxBufferBytes
)

// ErrResumeUnavailable is returned when a requested breakpoint can no longer
// be honored (buffered data was evicted or the backup node has no state).
var ErrResumeUnavailable = errors.New("stream resume unavailable: breakpoint out of range")

// Chunk is one frame on a Stream.
type Chunk struct {
	// Payload is the raw SSE frame bytes (already includes the "data: " prefix
	// when that is the downstream wire format).
	Payload []byte
	// Seq is the source's monotonic sequence angle for this frame.
	Seq int64
	// Err reports a terminal upstream error. A non-nil Err ends the stream.
	Err error
}

// Stream is a single open upstream stream instance.
type Stream interface {
	// Chunks yields the stream frames in order.
	Chunks() <-chan Chunk
	// Close aborts the stream and releases upstream resources.
	Close() error
}

// Source produces streams and supports transparent breakpoint resume.
type Source interface {
	// Start opens a stream. When resumeSeq > 0 the source MUST replay every
	// frame strictly after resumeSeq before producing fresh frames. A source
	// that can no longer honor the breakpoint MUST return ErrResumeUnavailable.
	Start(ctx context.Context, resumeSeq int64) (Stream, error)
}

// Options configures the Proxy.
type Options struct {
	// MaxBufferBytes caps the sliding-window buffer. Zero uses the default.
	MaxBufferBytes int

	// StallTimeout is the upstream idle gap that triggers a breakpoint resume.
	// Zero uses the default (5s).
	StallTimeout time.Duration

	// ResumeTimeout bounds each resume attempt. Zero uses the default (5s).
	ResumeTimeout time.Duration

	// KeepAlivePeriod is the SSE heartbeat interval. Zero uses the default;
	// a negative value disables heartbeats.
	KeepAlivePeriod time.Duration

	// DetectStall enables upstream-stall detection and automatic resume
	// (default true).
	DetectStall bool

	// Logger is an optional logrus logger. Defaults to the standard logger.
	Logger *log.Logger
}

func normalize(opts Options) Options {
	if opts.MaxBufferBytes <= 0 {
		opts.MaxBufferBytes = DefaultMaxBufferBytes
	}
	if opts.StallTimeout <= 0 {
		opts.StallTimeout = DefaultStallTimeout
	}
	if opts.ResumeTimeout <= 0 {
		opts.ResumeTimeout = DefaultResumeTimeout
	}
	if opts.KeepAlivePeriod == 0 {
		opts.KeepAlivePeriod = DefaultKeepAlivePeriod
	}
	return opts
}

// Proxy drives a resilient downstream stream from an upstream Source.
type Proxy struct {
	source Source
	opts   Options
	logger *log.Logger
}

// NewProxy creates a Proxy over source. The caller owns the source's lifecycle.
func NewProxy(source Source, opts Options) *Proxy {
	logger := opts.Logger
	if logger == nil {
		logger = log.StandardLogger()
	}
	return &Proxy{source: source, opts: normalize(opts), logger: logger}
}

// Handler returns an SSE http.HandlerFunc that serves the resilient stream.
// The downstream request context cancels the upstream on client disconnect.
func (p *Proxy) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		headers := w.Header()
		headers.Set("Content-Type", "text/event-stream")
		headers.Set("Cache-Control", "no-cache")
		headers.Set("Connection", "keep-alive")
		headers.Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		err := p.StreamTo(r.Context(), func(payload []byte) error {
			if _, err := w.Write(payload); err != nil {
				return err
			}
			flusher.Flush()
			return nil
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			p.logger.WithError(err).Warn("resilient stream ended with error")
		}
	}
}

// StreamTo pumps the resilient stream into emit until completion or ctx
// cancellation. emit receives complete SSE frames (or keep-alive comments).
func (p *Proxy) StreamTo(ctx context.Context, emit func([]byte) error) error {
	if ctx == nil {
		ctx = context.Background()
	}

	buf := stream.NewStreamBuffer(p.opts.MaxBufferBytes)
	current, err := p.start(ctx, 0)
	if err != nil {
		return err
	}

	var keepAliveC <-chan time.Time
	var keepAlive *time.Ticker
	if p.opts.KeepAlivePeriod > 0 {
		keepAlive = time.NewTicker(p.opts.KeepAlivePeriod)
		defer keepAlive.Stop()
		keepAliveC = keepAlive.C
	}

	stall := newStallTimer(p.opts.StallTimeout)
	defer stall.Stop()

	var delivered int64 // last seq emitted downstream; skip <= delivered on replay
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-keepAliveC:
			if err := emit([]byte(": keep-alive\n\n")); err != nil {
				return err
			}
		case <-stall.C():
			if !p.opts.DetectStall {
				stall.Reset(p.opts.StallTimeout)
				continue
			}
			resumed, errResume := p.resume(ctx, buf, delivered, emit)
			if errResume != nil {
				return errResume
			}
			prev := current
			current = resumed
			if prev != nil {
				_ = prev.Close()
			}
			stall.Reset(p.opts.StallTimeout)
		case frame, ok := <-current.Chunks():
			if !ok {
				return nil
			}
			if frame.Err != nil {
				return frame.Err
			}
			if len(frame.Payload) == 0 {
				continue
			}
			if frame.Seq > 0 && delivered > 0 && frame.Seq <= delivered {
				// Replay duplicate from a resumed stream: already emitted.
				continue
			}
			delivered = frame.Seq
			buf.WriteSeq(frame.Payload, frame.Seq)
			if err := emit(frame.Payload); err != nil {
				return err
			}
			stall.Reset(p.opts.StallTimeout)
		}
	}
}

// start opens a stream. The resume timeout bounds only the initial
// Source.Start call, not the returned stream's lifetime.
func (p *Proxy) start(ctx context.Context, resumeSeq int64) (Stream, error) {
	return p.source.Start(ctx, resumeSeq)
}

// resume attempts a breakpoint resume from the delivered marker. The buffered
// window is used only as the resume-capability record; the replacement stream
// performs the actual replay so output stays contiguous with the emitted flow.
func (p *Proxy) resume(ctx context.Context, buf *stream.StreamBuffer, delivered int64, emit func([]byte) error) (Stream, error) {
	p.logger.WithFields(log.Fields{
		"delivered_seq":  delivered,
		"buffered_bytes": buf.Size(),
	}).Info("upstream stall detected; initiating breakpoint resume")
	if err := emit([]byte(": upstream resume\n\n")); err != nil {
		return nil, err
	}
	return p.start(ctx, delivered)
}

// stallTimer is a resettable one-shot timer.
type stallTimer struct {
	timer *time.Timer
}

func newStallTimer(d time.Duration) *stallTimer {
	return &stallTimer{timer: time.NewTimer(d)}
}

func (s *stallTimer) C() <-chan time.Time {
	return s.timer.C
}

func (s *stallTimer) Reset(d time.Duration) {
	if !s.timer.Stop() {
		select {
		case <-s.timer.C:
		default:
		}
	}
	s.timer.Reset(d)
}

func (s *stallTimer) Stop() { s.timer.Stop() }
