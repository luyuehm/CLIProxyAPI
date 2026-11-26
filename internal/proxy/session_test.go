package proxy

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSplitSSEFramesPartial(t *testing.T) {
	first, rest := SplitSSEFrames([]byte("data: a\n\ndata: b\n\ndata: c"))
	if len(first) != 2 {
		t.Fatalf("complete frames = %d, want 2", len(first))
	}
	if string(first[0]) != "data: a\n\n" || string(first[1]) != "data: b\n\n" {
		t.Fatalf("unexpected frames: %q, %q", first[0], first[1])
	}
	if string(rest) != "data: c" {
		t.Fatalf("rest = %q, want %q", rest, "data: c")
	}
}

func TestSplitSSEFramesCRLF(t *testing.T) {
	frames, rest := SplitSSEFrames([]byte("data: a\r\n\r\ndata: b\r\n\r\n"))
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want 2", len(frames))
	}
	if len(rest) != 0 {
		t.Fatalf("rest = %q, want empty", rest)
	}
}

func TestWithEventIDReplacesUpstreamID(t *testing.T) {
	got := string(WithEventID([]byte("id: upstream-9\ndata: {\"x\":1}\n\n"), 42))
	want := "id: 42\ndata: {\"x\":1}\n\n"
	if got != want {
		t.Fatalf("WithEventID = %q, want %q", got, want)
	}
}

func TestWithEventIDKeepsCommentFrame(t *testing.T) {
	in := []byte(": keep-alive\n\n")
	if got := string(WithEventID(in, 7)); got != string(in) {
		t.Fatalf("comment frame changed: %q", got)
	}
}

func TestParseEventID(t *testing.T) {
	cases := []struct {
		raw  string
		want int64
		ok   bool
	}{
		{"500", 500, true},
		{" 12 ", 12, true},
		{"0", 0, true},
		{"", 0, false},
		{"abc", 0, false},
		{"-3", 0, false},
	}
	for _, tc := range cases {
		got, ok := ParseEventID(tc.raw)
		if got != tc.want || ok != tc.ok {
			t.Errorf("ParseEventID(%q) = (%d,%v), want (%d,%v)", tc.raw, got, ok, tc.want, tc.ok)
		}
	}
}

func newTestSession(t *testing.T, maxBytes int) *Session {
	t.Helper()
	registry := NewRegistry(RegistryOptions{MaxSessionBytes: maxBytes})
	t.Cleanup(registry.Close)
	session, err := registry.Open("key-a", func() {})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(session.Close)
	return session
}

func TestSessionAppendAssignsMonotonicSeq(t *testing.T) {
	session := newTestSession(t, 0)
	// A frame split across writes must still produce exactly one sequenced frame.
	session.Append([]byte("data: he"))
	frames := session.Append([]byte("llo\n\ndata: world\n\n"))
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want 2", len(frames))
	}
	if frames[0].Seq != 1 || frames[1].Seq != 2 {
		t.Fatalf("seqs = %d,%d, want 1,2", frames[0].Seq, frames[1].Seq)
	}
	if string(frames[0].Payload) != "data: hello\n\n" || string(frames[1].Payload) != "data: world\n\n" {
		t.Fatalf("payloads = %q, %q", frames[0].Payload, frames[1].Payload)
	}
}

// TestSessionReplayAfterBreakpoint is the AC1 core: a client that processed
// frames 1..N and reconnects with Last-Event-ID N receives exactly N+1..end,
// with no duplicate and no gap.
func TestSessionReplayAfterBreakpoint(t *testing.T) {
	session := newTestSession(t, 0)
	const total = 1000
	const breakpoint = 500
	for i := 1; i <= total; i++ {
		session.Append([]byte("data: " + itoa(i) + "\n\n"))
	}
	session.Finish(nil)

	sub, err := session.Subscribe(breakpoint)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	var got []string
	for {
		frame, errNext := sub.Next(context.Background())
		if errors.Is(errNext, io.EOF) {
			break
		}
		if errNext != nil {
			t.Fatalf("Next: %v", errNext)
		}
		if frame.Seq != int64(breakpoint+1+len(got)) {
			t.Fatalf("seq = %d at position %d, want %d", frame.Seq, len(got), breakpoint+1+len(got))
		}
		got = append(got, strings.TrimSpace(string(frame.Payload)))
	}
	if len(got) != total-breakpoint {
		t.Fatalf("replayed %d frames, want %d", len(got), total-breakpoint)
	}
	for i, payload := range got {
		want := "data: " + itoa(breakpoint+1+i)
		if payload != want {
			t.Fatalf("frame %d = %q, want %q", i, payload, want)
		}
	}
}

// TestSessionReplayDetectsGap proves the consistency guarantee: when the
// requested breakpoint fell out of the retention window the session refuses to
// serve a continuation instead of silently skipping frames.
func TestSessionReplayDetectsGap(t *testing.T) {
	session := newTestSession(t, 60) // roughly two 30-byte frames retained
	for i := 1; i <= 10; i++ {
		session.Append([]byte("data: " + itoa(i) + "\n\n"))
	}
	if _, err := session.Subscribe(1); !errors.Is(err, ErrResumeGap) {
		t.Fatalf("Subscribe(1) error = %v, want ErrResumeGap", err)
	}
	// The newest breakpoints are still served.
	if _, err := session.Subscribe(session.LastSeq() - 1); err != nil {
		t.Fatalf("Subscribe(newest) error = %v, want nil", err)
	}
}

func TestSessionSubscribeRejectsFutureSeq(t *testing.T) {
	session := newTestSession(t, 0)
	session.Append([]byte("data: a\n\n"))
	if _, err := session.Subscribe(99); !errors.Is(err, ErrResumeGap) {
		t.Fatalf("error = %v, want ErrResumeGap", err)
	}
}

func TestSessionOwnership(t *testing.T) {
	session := newTestSession(t, 0)
	if !session.OwnedBy("key-a") {
		t.Fatal("session should be owned by its opening key")
	}
	if session.OwnedBy("key-b") {
		t.Fatal("session must not be owned by a different key")
	}
	if session.OwnedBy("") {
		t.Fatal("session must not be owned by an empty key")
	}
}

func TestSessionLiveFollowAfterTerminalFrames(t *testing.T) {
	session := newTestSession(t, 0)
	sub, err := session.Subscribe(0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 1; i <= 3; i++ {
			session.Append([]byte("data: " + itoa(i) + "\n\n"))
		}
		session.Finish(nil)
	}()

	var seqs []int64
	for {
		frame, errNext := sub.Next(context.Background())
		if errors.Is(errNext, io.EOF) {
			break
		}
		if errNext != nil {
			t.Fatalf("Next: %v", errNext)
		}
		seqs = append(seqs, frame.Seq)
	}
	<-done
	if len(seqs) != 3 {
		t.Fatalf("received %v, want 3 frames", seqs)
	}
}

func TestSessionFlushPending(t *testing.T) {
	session := newTestSession(t, 0)
	session.Append([]byte("data: tail"))
	frame, ok := session.FlushPending()
	if !ok {
		t.Fatal("expected a pending frame")
	}
	if string(frame.Payload) != "data: tail" || frame.Seq != 1 {
		t.Fatalf("frame = %+v", frame)
	}
	if _, ok := session.FlushPending(); ok {
		t.Fatal("pending buffer should be empty after flush")
	}
}

func TestSessionConcurrentProducerAndSubscribers(t *testing.T) {
	session := newTestSession(t, 0)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sub, err := session.Subscribe(0)
			if err != nil {
				t.Errorf("Subscribe: %v", err)
				return
			}
			defer sub.Close()
			for {
				_, errNext := sub.Next(context.Background())
				if errors.Is(errNext, io.EOF) {
					return
				}
				if errNext != nil {
					t.Errorf("Next: %v", errNext)
					return
				}
			}
		}()
	}
	for i := 0; i < 200; i++ {
		session.Append([]byte("data: " + itoa(i) + "\n\n"))
	}
	session.Finish(nil)
	wg.Wait()
	if got := session.LastSeq(); got != 200 {
		t.Fatalf("LastSeq = %d, want 200", got)
	}
}

func TestRegistryFingerprintStable(t *testing.T) {
	registry := NewRegistry(RegistryOptions{})
	defer registry.Close()
	first := registry.Fingerprint("sk-abc")
	second := registry.Fingerprint("sk-abc")
	if first != second {
		t.Fatal("fingerprint must be stable for the same key")
	}
	if first == registry.Fingerprint("sk-abd") {
		t.Fatal("different keys must produce different fingerprints")
	}
	if strings.Contains(first, "sk-") {
		t.Fatal("fingerprint must not embed the plaintext key")
	}
}

func TestRegistryIdleSweepReleasesSession(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	registry := NewRegistry(RegistryOptions{
		IdleTTL:       time.Minute,
		SweepInterval: time.Hour, // drive sweeps manually
		Now:           func() time.Time { return now },
	})
	defer registry.Close()

	cancelled := make(chan struct{})
	session, err := registry.Open("k", func() { close(cancelled) })
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	session.Append([]byte("data: a\n\n"))

	registry.sweep()
	if registry.Len() != 1 {
		t.Fatalf("session swept before TTL: %d", registry.Len())
	}

	now = now.Add(2 * time.Minute)
	registry.sweep()
	if registry.Len() != 0 {
		t.Fatalf("session not swept after TTL: %d", registry.Len())
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("sweeping an expired session must cancel its upstream")
	}
}

func TestRegistryHeldSessionSurvivesSweep(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	registry := NewRegistry(RegistryOptions{
		IdleTTL:       time.Minute,
		SweepInterval: time.Hour,
		Now:           func() time.Time { return now },
	})
	defer registry.Close()

	session, err := registry.Open("k", func() {})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	session.Hold()
	now = now.Add(10 * time.Minute)
	registry.sweep()
	if registry.Len() != 1 {
		t.Fatal("a held session must never be swept")
	}
	session.Release()
	// Releasing refreshes last-activity, so the session gets a fresh idle window.
	now = now.Add(2 * time.Minute)
	registry.sweep()
	if registry.Len() != 0 {
		t.Fatal("released idle session should be swept")
	}
}

func TestRegistryPerKeyCap(t *testing.T) {
	registry := NewRegistry(RegistryOptions{MaxSessionsPerKey: 2, SweepInterval: time.Hour})
	defer registry.Close()

	first, err := registry.Open("k", func() {})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	first.Append([]byte("data: a\n\n"))
	if _, err := registry.Open("k", func() {}); err != nil {
		t.Fatalf("Open second: %v", err)
	}
	third, err := registry.Open("k", func() {})
	if err != nil {
		t.Fatalf("Open third: %v", err)
	}
	if registry.Len() != 2 {
		t.Fatalf("Len = %d, want 2", registry.Len())
	}
	if _, ok := registry.Lookup(first.ID()); ok {
		t.Fatal("oldest session should have been evicted")
	}
	if _, ok := registry.Lookup(third.ID()); !ok {
		t.Fatal("newest session must be retained")
	}
}

func TestRegistryPerKeyCapRejectsWhenAllHeld(t *testing.T) {
	registry := NewRegistry(RegistryOptions{MaxSessionsPerKey: 1, SweepInterval: time.Hour})
	defer registry.Close()

	session, err := registry.Open("k", func() {})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	session.Hold()
	if _, err := registry.Open("k", func() {}); !errors.Is(err, ErrTooManyStreams) {
		t.Fatalf("error = %v, want ErrTooManyStreams", err)
	}
}

func TestRegistryCloseReleasesSessions(t *testing.T) {
	registry := NewRegistry(RegistryOptions{SweepInterval: time.Hour})
	session, err := registry.Open("k", func() {})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	registry.Close()
	if !session.Closed() {
		t.Fatal("Close must close retained sessions")
	}
}

func TestSessionWriteNeverFailsAfterClose(t *testing.T) {
	session := newTestSession(t, 0)
	session.Close()
	// A producer that outlives retention must keep draining, not fail midway.
	if _, err := session.Write([]byte("data: late\n\n")); err != nil {
		t.Fatalf("Write after close = %v, want nil", err)
	}
	if frames := session.Append([]byte("data: late\n\n")); frames != nil {
		t.Fatalf("Append after close = %v, want nil", frames)
	}
}

func itoa(v int) string {
	return strconv.Itoa(v)
}

// TestSessionLargeReplayIsExact checks the no-gap/no-duplicate guarantee at a
// size where an off-by-one in the window or subscription index would show.
func TestSessionLargeReplayIsExact(t *testing.T) {
	session := newTestSession(t, 0) // default window holds the whole run
	const total = 20000
	writer := make([]byte, 0, 1<<20)
	for i := 1; i <= total; i++ {
		writer = append(writer, []byte("data: "+strconv.Itoa(i)+"\n\n")...)
		// Feed in irregular slices so frame splitting is exercised.
		if i%97 == 0 {
			session.Append(writer)
			writer = writer[:0]
		}
	}
	if len(writer) > 0 {
		session.Append(writer)
	}
	session.Finish(nil)
	if got := session.LastSeq(); got != total {
		t.Fatalf("LastSeq = %d, want %d", got, total)
	}

	for _, breakpoint := range []int64{0, 1, 9999, total - 1, total} {
		sub, err := session.Subscribe(breakpoint)
		if err != nil {
			t.Fatalf("Subscribe(%d): %v", breakpoint, err)
		}
		var count int64
		var next int64 = breakpoint + 1
		for {
			frame, errNext := sub.Next(context.Background())
			if errors.Is(errNext, io.EOF) {
				break
			}
			if errNext != nil {
				t.Fatalf("Subscribe(%d) Next: %v", breakpoint, errNext)
			}
			if frame.Seq != next {
				t.Fatalf("Subscribe(%d): seq %d, want %d", breakpoint, frame.Seq, next)
			}
			next++
			count++
		}
		sub.Close()
		if want := int64(total) - breakpoint; count != want {
			t.Fatalf("Subscribe(%d) replayed %d frames, want %d", breakpoint, count, want)
		}
	}
}
