package contentfilter

import (
	"bytes"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// maxBufferedResponse caps how large a non-streaming response body the filter
// will hold in memory before bypassing. Oversized bodies pass through
// untouched to avoid unbounded memory use.
const maxBufferedResponse = 32 << 20 // 32 MiB

// filterResponseWriter wraps gin.ResponseWriter to apply outbound masking.
// Non-streaming responses are buffered and masked as a whole after the
// handler completes; SSE (text/event-stream) responses are masked per event
// as they are written, so streaming stays low-latency.
type filterResponseWriter struct {
	gin.ResponseWriter
	engine *Engine
	rules  []*Rule
	model  string

	status       int
	headerCalled bool
	wantsStream  bool // request asked for a streaming (SSE) response
	bypassed     bool // content cannot be filtered (gzip etc.)
	buffered     bytes.Buffer
	streamer     *streamMasker
}

// isStream reports whether the response is an SSE stream, based on the
// negotiated streaming request and/or the Content-Type set by the handler.
func (w *filterResponseWriter) isStream() bool {
	if w.wantsStream {
		return true
	}
	for k, v := range w.Header() {
		if strings.EqualFold(k, "Content-Type") && len(v) > 0 && strings.Contains(strings.ToLower(v[0]), "text/event-stream") {
			return true
		}
	}
	return false
}

// checkBypass detects payloads the filter cannot safely rewrite (compressed
// responses). It deploys the pass-through mode on first write.
func (w *filterResponseWriter) checkBypass() {
	for k, v := range w.Header() {
		if strings.EqualFold(k, "Content-Encoding") && len(v) > 0 {
			enc := strings.ToLower(strings.TrimSpace(v[0]))
			if enc != "" && enc != "identity" {
				w.bypassed = true
			}
		}
	}
}

// WriteHeader records the status code. For streaming responses it emits the
// header immediately so SSE framing is untouched; for non-streaming responses
// header emission is deferred until the buffered body is flushed (finalize).
func (w *filterResponseWriter) WriteHeader(code int) {
	if w.headerCalled {
		return
	}
	w.status = code
	if w.isStream() {
		w.headerCalled = true
		w.ensureStreamer()
		w.ResponseWriter.WriteHeader(code)
	}
}

// ensureStreamer initializes the per-stream masker lazily so plain responses
// never pay for it.
func (w *filterResponseWriter) ensureStreamer() {
	if w.streamer != nil {
		return
	}
	w.streamer = &streamMasker{
		engine: w.engine,
		rules:  w.rules,
		model:  w.model,
		out:    w.ResponseWriter,
	}
}

// Write buffers or streams the response body. Streaming writes are masked
// event-by-event; non-streaming writes accumulate until finalize.
func (w *filterResponseWriter) Write(p []byte) (int, error) {
	w.checkBypass()
	if !w.headerCalled {
		if w.isStream() {
			w.headerCalled = true
			w.ensureStreamer()
			w.ResponseWriter.WriteHeader(w.statusOrOK())
		} else if w.bypassed {
			w.headerCalled = true
			w.ResponseWriter.WriteHeader(w.statusOrOK())
			return w.ResponseWriter.Write(p)
		}
	}
	if w.streamer != nil {
		n, err := w.streamer.Write(p)
		w.flush()
		return n, err
	}
	if w.bypassed {
		return w.ResponseWriter.Write(p)
	}
	if w.buffered.Len()+len(p) > maxBufferedResponse {
		// Oversized body: stop buffering, flush what we have unmasked, and
		// pass through the remainder. Avoids unbounded memory.
		w.flushBuffered()
		return w.ResponseWriter.Write(p)
	}
	return w.buffered.Write(p)
}

func (w *filterResponseWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

func (w *filterResponseWriter) flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// statusOrOK returns the recorded status, defaulting to 200.
func (w *filterResponseWriter) statusOrOK() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

// flushBuffered emits the buffered bytes to the underlying writer unmasked.
func (w *filterResponseWriter) flushBuffered() {
	w.ensureHeader()
	_, _ = w.buffered.WriteTo(w.ResponseWriter)
}

// ensureHeader emits the response header once if it has not been emitted yet.
func (w *filterResponseWriter) ensureHeader() {
	if w.headerCalled {
		return
	}
	w.headerCalled = true
	w.ResponseWriter.WriteHeader(w.statusOrOK())
}

// finalize flushes buffered non-streaming output through the engine and writes
// it to the client. It is called by the middleware after c.Next(). Streaming
// writers flush whatever remains buffered as a partial trailing event.
func (w *filterResponseWriter) finalize() {
	if w.streamer != nil {
		w.ensureHeader()
		w.streamer.Close()
		w.flush()
		return
	}
	if w.bypassed || w.buffered.Len() == 0 {
		w.ensureHeader()
		return
	}
	body := w.buffered.Bytes()
	masked := w.engine.Apply(w.rules, string(body), false, w.model).Text
	if len(body) != len(masked) {
		// Length changed; the Content-Length header would be stale.
		w.Header().Del("Content-Length")
	}
	w.ensureHeader()
	if masked != string(body) {
		_, _ = io.WriteString(w.ResponseWriter, masked)
	} else {
		_, _ = w.buffered.WriteTo(w.ResponseWriter)
	}
}
