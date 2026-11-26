package middleware

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/proxy"
	log "github.com/sirupsen/logrus"
)

const (
	// HeaderStreamResume opts a first request into resumable streaming.
	HeaderStreamResume = "X-Stream-Resume"
	// HeaderStreamID returns the stream identifier and selects it on resume.
	HeaderStreamID = "X-Stream-Id"
	// HeaderLastEventID carries the last processed event id on resume.
	HeaderLastEventID = "Last-Event-ID"
	// headerLastEventIDAlt is accepted for clients that cannot set Last-Event-ID.
	headerLastEventIDAlt = "X-Last-Event-Id"

	ctxStreamResumeKey = "STREAM_RESUME_SESSION"
)

// StreamResume returns a per-route middleware that adds resumable streaming to
// an SSE handler. It is a no-op for requests that do not opt in.
//
// Protocol: a first request sends "X-Stream-Resume: 1"; the gateway returns an
// unguessable X-Stream-Id and tags every SSE event with a monotonic "id:" field.
// The upstream keeps producing into a detached session after the client drops.
// To continue, the client re-sends the same request with X-Stream-Id and
// Last-Event-ID; it receives exactly the events it missed, with no gap and no
// duplicate. The session is bound to the API key that opened it, and a resume
// attempt from another key is rejected with 403 before any buffered content is
// written.
func StreamResume(registry *proxy.Registry) gin.HandlerFunc {
	return func(c *gin.Context) {
		if registry == nil || c.Request == nil {
			c.Next()
			return
		}
		keyID := registry.Fingerprint(c.GetString("userApiKey"))

		if streamID := strings.TrimSpace(c.GetHeader(HeaderStreamID)); streamID != "" {
			serveResume(c, registry, streamID, keyID)
			return
		}
		if !resumeRequested(c) {
			c.Next()
			return
		}
		serveResumable(c, registry, keyID)
	}
}

// CurrentStreamID returns the stream identifier of the response being served,
// when the request opted into resumable streaming. Handlers may expose it in
// their own bodies; the gateway already returns it in the X-Stream-Id header.
func CurrentStreamID(c *gin.Context) (string, bool) {
	if c == nil {
		return "", false
	}
	session, ok := c.Get(ctxStreamResumeKey)
	if !ok {
		return "", false
	}
	resumeSession, ok := session.(*proxy.Session)
	if !ok || resumeSession == nil {
		return "", false
	}
	return resumeSession.ID(), true
}

func resumeRequested(c *gin.Context) bool {
	raw := strings.TrimSpace(c.GetHeader(HeaderStreamResume))
	if raw == "" {
		return false
	}
	switch strings.ToLower(raw) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

// serveResumable runs the handler while tee-ing its SSE output into a session.
func serveResumable(c *gin.Context, registry *proxy.Registry, keyID string) {
	origCtx := c.Request.Context()
	// Detach the producer from the client connection: a network drop must not
	// abort the upstream generation, or there would be nothing to resume.
	detachedCtx, cancelDetached := context.WithCancel(context.WithoutCancel(origCtx))

	session, errSession := registry.Open(keyID, cancelDetached)
	if errSession != nil {
		cancelDetached()
		status := http.StatusTooManyRequests
		if errors.Is(errSession, proxy.ErrTooManyStreams) {
			c.AbortWithStatusJSON(status, gin.H{"error": gin.H{
				"message": "too many concurrent streams for this API key",
				"type":    "rate_limit_error",
			}})
			return
		}
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": gin.H{
			"message": "stream resume unavailable",
			"type":    "server_error",
		}})
		return
	}
	session.Hold()
	c.Set(ctxStreamResumeKey, session)
	c.Request = c.Request.WithContext(detachedCtx)

	writer := &resumeWriter{ResponseWriter: c.Writer, session: session}
	c.Writer = writer

	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(session.Release) }
	// Deferred so a panicking handler (recovered upstream by the engine) cannot
	// leave the session held forever and outside the idle sweep.
	defer release()
	producerDone := make(chan struct{})
	// If the client vanishes while the handler is still producing, stop holding
	// the session so the idle sweep can reclaim it if the upstream stalls.
	go func() {
		select {
		case <-origCtx.Done():
			release()
		case <-producerDone:
		}
	}()

	c.Next()

	close(producerDone)
	release()

	if !writer.streaming {
		// The response was not an SSE stream (validation error, JSON body, ...):
		// there is nothing to resume.
		session.Close()
		return
	}
	if frame, ok := session.FlushPending(); ok {
		_, _ = writer.ResponseWriter.Write(proxy.WithEventID(frame.Payload, frame.Seq))
	}
	writer.ResponseWriter.Flush()
	session.Finish(nil)
}

// serveResume streams the events a client missed from an existing session.
func serveResume(c *gin.Context, registry *proxy.Registry, streamID, keyID string) {
	session, ok := registry.Lookup(streamID)
	if !ok {
		abortStreamError(c, http.StatusGone, "stream_not_found", "stream is unknown or expired; restart the request")
		return
	}
	// Ownership is verified before any buffered byte is written. The error body
	// is identical for every mismatch so it never leaks the stream's content or
	// even whether other keys' payloads would have been available.
	if !session.OwnedBy(keyID) {
		abortStreamError(c, http.StatusForbidden, "stream_forbidden", "this stream belongs to a different API key")
		return
	}

	afterSeq, _ := proxy.ParseEventID(c.GetHeader(HeaderLastEventID))
	if raw := c.GetHeader(HeaderLastEventID); raw == "" {
		afterSeq, _ = proxy.ParseEventID(c.GetHeader(headerLastEventIDAlt))
	}

	sub, errSub := session.Subscribe(afterSeq)
	if errSub != nil {
		switch {
		case errors.Is(errSub, proxy.ErrResumeGap):
			abortStreamError(c, http.StatusConflict, "stream_resume_gap", "breakpoint is no longer retained; restart the request")
		case errors.Is(errSub, proxy.ErrSessionClosed):
			abortStreamError(c, http.StatusGone, "stream_not_found", "stream is unknown or expired; restart the request")
		default:
			abortStreamError(c, http.StatusInternalServerError, "stream_resume_failed", "stream resume failed")
		}
		return
	}
	defer sub.Close()

	headers := c.Writer.Header()
	headers.Set("Content-Type", "text/event-stream")
	headers.Set("Cache-Control", "no-cache")
	headers.Set("Connection", "keep-alive")
	headers.Set("X-Accel-Buffering", "no")
	headers.Set(HeaderStreamID, session.ID())
	c.Status(http.StatusOK)
	c.Writer.Flush()
	c.Abort()

	ctx := c.Request.Context()
	for {
		frame, errNext := sub.Next(ctx)
		if errNext != nil {
			if errors.Is(errNext, io.EOF) || errors.Is(errNext, context.Canceled) {
				return
			}
			if errors.Is(errNext, context.DeadlineExceeded) {
				return
			}
			// Headers are already committed; report the terminal condition
			// in-band so the client knows it must restart.
			_, _ = c.Writer.Write([]byte("event: error\ndata: {\"error\":{\"type\":\"stream_resume_failed\",\"message\":\"stream resume interrupted\"}}\n\n"))
			c.Writer.Flush()
			log.WithError(errNext).WithField("stream_id", session.ID()).Warn("stream resume ended early")
			return
		}
		if _, errWrite := c.Writer.Write(proxy.WithEventID(frame.Payload, frame.Seq)); errWrite != nil {
			return
		}
		c.Writer.Flush()
	}
}

func abortStreamError(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{
		"message": message,
		"type":    "invalid_request_error",
		"code":    code,
	}})
}

// resumeWriter tees the handler's SSE output into the session and forwards it
// to the live client. Frame boundaries are decided here, not by the handler, so
// the message may arrive in arbitrary write sizes.
type resumeWriter struct {
	gin.ResponseWriter
	session   *proxy.Session
	streaming bool
	decided   bool
}

func (w *resumeWriter) decide(code int) {
	if w.decided {
		return
	}
	w.decided = true
	if code != http.StatusOK {
		return
	}
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(w.Header().Get("Content-Type"))), "text/event-stream") {
		return
	}
	w.streaming = true
	w.Header().Set(HeaderStreamID, w.session.ID())
}

func (w *resumeWriter) WriteHeader(code int) {
	w.decide(code)
	w.ResponseWriter.WriteHeader(code)
}

func (w *resumeWriter) WriteHeaderNow() {
	w.decide(http.StatusOK)
	w.ResponseWriter.WriteHeaderNow()
}

func (w *resumeWriter) Write(p []byte) (int, error) {
	if !w.decided {
		w.decide(http.StatusOK)
	}
	if !w.streaming {
		return w.ResponseWriter.Write(p)
	}
	w.forward(p)
	return len(p), nil
}

func (w *resumeWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

// forward retains the bytes in the session and writes the frames they complete
// to the client. Write errors are swallowed on purpose: the producer must keep
// going after the client disconnects so the session has a tail to resume from.
func (w *resumeWriter) forward(p []byte) {
	frames := w.session.Append(p)
	for _, frame := range frames {
		if _, errWrite := w.ResponseWriter.Write(proxy.WithEventID(frame.Payload, frame.Seq)); errWrite != nil {
			return
		}
	}
	w.ResponseWriter.Flush()
}
