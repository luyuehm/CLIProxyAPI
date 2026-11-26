package proxy

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"sort"
	"sync"
	"time"
)

var (
	// ErrResumeGap is returned when the requested breakpoint was already evicted
	// from the retention window, so the client cannot be served a gap-free
	// continuation and must restart the request.
	ErrResumeGap = errors.New("stream resume unavailable: breakpoint already evicted")

	// ErrSessionClosed is returned when a session was closed (expired, evicted,
	// or the upstream finished and the retention window lapsed).
	ErrSessionClosed = errors.New("stream session closed")

	// ErrSessionMismatch is returned when a resume attempt presents a session
	// ID that is bound to a different API key.
	ErrSessionMismatch = errors.New("stream session credential mismatch")
)

// Frame is one complete SSE event retained for replay.
type Frame struct {
	// Seq is the gateway-assigned monotonic sequence number (1-based).
	Seq int64
	// Payload is the raw SSE frame including its terminating blank line.
	Payload []byte
}

// Session buffers the frames of one streaming response so a client that lost
// its connection can resume from the last event it processed.
//
// A Session is bound to the key fingerprint of the API key that opened it;
// resuming requires presenting a key with the same fingerprint. Frames are
// retained in a byte-bounded window, so memory stays bounded for very long
// generations while any breakpoint inside the window remains resumable.
type Session struct {
	id    string
	keyID string

	mu       sync.Mutex
	frames   []Frame
	firstSeq int64
	nextSeq  int64
	bytes    int
	maxBytes int
	pending  []byte

	terminal bool
	termErr  error
	closed   bool
	changed  chan struct{}

	subscribers int
	holds       int
	now         func() time.Time
	createdAt   time.Time
	lastActive  time.Time

	cancel  context.CancelFunc
	onClose func(*Session)
}

// clock returns the session clock, defaulting to the wall clock.
func (s *Session) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// ID returns the unguessable session identifier.
func (s *Session) ID() string { return s.id }

// OwnedBy reports whether keyID is the fingerprint that opened the session.
// The comparison is constant time so a mismatch leaks no timing signal.
func (s *Session) OwnedBy(keyID string) bool {
	if s == nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(s.keyID), []byte(keyID)) == 1
}

// Write appends raw response bytes to the session. See Append.
func (s *Session) Write(p []byte) (int, error) {
	s.Append(p)
	return len(p), nil
}

// Append buffers raw response bytes, retaining each complete SSE frame, and
// returns the frames that became complete because of this write. Partial
// trailing bytes are held until a later write completes them. It never returns
// an error for a closed session: a producer that outlived its retention window
// must keep draining its upstream instead of failing midway.
func (s *Session) Append(p []byte) []Frame {
	if len(p) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	combined := p
	if len(s.pending) > 0 {
		combined = make([]byte, 0, len(s.pending)+len(p))
		combined = append(combined, s.pending...)
		combined = append(combined, p...)
	}
	frames, rest := SplitSSEFrames(combined)
	appended := make([]Frame, 0, len(frames))
	for _, frame := range frames {
		appended = append(appended, s.appendLocked(frame))
	}
	if len(rest) > 0 {
		s.pending = append(s.pending[:0], rest...)
	} else {
		s.pending = nil
	}
	return appended
}

// FlushPending appends the buffered partial frame, if any, as a frame so no
// bytes are lost when the upstream ends without a trailing blank line.
func (s *Session) FlushPending() (Frame, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || len(s.pending) == 0 {
		return Frame{}, false
	}
	frame := s.appendLocked(s.pending)
	s.pending = nil
	return frame, true
}

// appendLocked stores one frame, assigns its sequence number, trims the window
// to the byte budget, and wakes subscribers. Callers hold s.mu.
func (s *Session) appendLocked(payload []byte) Frame {
	s.nextSeq++
	frame := Frame{Seq: s.nextSeq, Payload: append([]byte(nil), payload...)}
	s.frames = append(s.frames, frame)
	s.bytes += len(frame.Payload)
	s.lastActive = s.clock()
	if s.firstSeq == 0 {
		s.firstSeq = frame.Seq
	}
	for s.bytes > s.maxBytes && len(s.frames) > 1 {
		s.bytes -= len(s.frames[0].Payload)
		s.frames = s.frames[1:]
		s.firstSeq = s.frames[0].Seq
	}
	s.broadcastLocked()
	return frame
}

// Finish marks the upstream as complete. Subscribers drain the retained frames
// and then observe io.EOF. The frames stay resumable until the retention window
// lapses so a client can still recover a tail it did not receive.
func (s *Session) Finish(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.terminal || s.closed {
		return
	}
	s.terminal = true
	s.termErr = err
	s.lastActive = s.clock()
	s.broadcastLocked()
}

// Terminal reports whether the upstream finished and its terminal error.
func (s *Session) Terminal() (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.terminal, s.termErr
}

// Close releases the session, cancels the detached upstream execution and
// removes it from its registry. It is idempotent.
func (s *Session) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	cancel := s.cancel
	onClose := s.onClose
	s.frames = nil
	s.pending = nil
	s.broadcastLocked()
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if onClose != nil {
		onClose(s)
	}
}

// Closed reports whether the session was released.
func (s *Session) Closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// LastSeq returns the sequence number of the newest retained frame (0 when none).
func (s *Session) LastSeq() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nextSeq
}

// Size returns the retained byte count.
func (s *Session) Size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bytes
}

// Subscribers returns the number of clients currently attached.
func (s *Session) Subscribers() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.subscribers
}

// Hold pins the session to the producer that is still filling it. A held
// session is never reclaimed by the idle sweep; the producer releases the hold
// when it stops writing.
func (s *Session) Hold() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.holds++
}

// Release drops one hold taken by Hold.
func (s *Session) Release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.holds > 0 {
		s.holds--
		s.lastActive = s.clock()
	}
}

// Held reports whether a producer still holds the session.
func (s *Session) Held() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.holds > 0
}

// LastActive returns the last time the session produced or finished a frame.
func (s *Session) LastActive() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastActive
}

// IdleFor reports how long the session has been idle at now.
func (s *Session) IdleFor(now time.Time) time.Duration {
	return now.Sub(s.LastActive())
}

// broadcastLocked closes and replaces the change channel, waking every waiter.
func (s *Session) broadcastLocked() {
	if s.changed == nil {
		s.changed = make(chan struct{})
		return
	}
	close(s.changed)
	s.changed = make(chan struct{})
}

// Subscribe attaches a reader that first replays every retained frame with
// Seq > afterSeq and then follows the live stream. afterSeq == 0 starts from
// the oldest retained frame. ErrResumeGap is returned when the requested
// breakpoint is no longer served contiguously.
func (s *Session) Subscribe(afterSeq int64) (*Subscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrSessionClosed
	}
	if afterSeq < 0 {
		return nil, ErrResumeGap
	}
	if afterSeq > s.nextSeq {
		// The client claims a position the server never produced.
		return nil, ErrResumeGap
	}
	if afterSeq+1 < s.firstSeq {
		// Frames between the breakpoint and the retained window were evicted.
		return nil, ErrResumeGap
	}
	s.subscribers++
	return &Subscription{sess: s, last: afterSeq}, nil
}

// Subscription is one attached reader over a Session.
type Subscription struct {
	sess   *Session
	last   int64
	closed bool
}

// Next returns the next frame after the reader position, blocking until a frame
// arrives, the stream finishes (io.EOF), the session closes (ErrSessionClosed),
// or ctx is done.
func (sub *Subscription) Next(ctx context.Context) (Frame, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	s := sub.sess
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return Frame{}, ErrSessionClosed
		}
		if frames := s.framesAfter(sub.last); len(frames) > 0 {
			frame := frames[0]
			sub.last = frame.Seq
			s.mu.Unlock()
			return frame, nil
		}
		if sub.last+1 < s.firstSeq {
			s.mu.Unlock()
			return Frame{}, ErrResumeGap
		}
		if s.terminal {
			termErr := s.termErr
			s.mu.Unlock()
			if termErr != nil {
				return Frame{}, termErr
			}
			return Frame{}, io.EOF
		}
		changed := s.changed
		s.mu.Unlock()

		select {
		case <-changed:
		case <-ctx.Done():
			return Frame{}, ctx.Err()
		}
	}
}

// Close detaches the reader. It is idempotent.
func (sub *Subscription) Close() {
	sub.sess.mu.Lock()
	if !sub.closed {
		sub.closed = true
		sub.sess.subscribers--
	}
	sub.sess.mu.Unlock()
}

// framesAfter returns the retained frames with Seq > last. The window is
// ordered by Seq, so a binary search keeps a long replay linear overall.
func (s *Session) framesAfter(last int64) []Frame {
	idx := sort.Search(len(s.frames), func(i int) bool { return s.frames[i].Seq > last })
	return s.frames[idx:]
}
