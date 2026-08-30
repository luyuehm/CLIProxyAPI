package contentfilter

import (
	"bufio"
	"bytes"
	"io"
)

// filterStream reads an SSE response from r and writes the masked version to w.
//
// SSE events are delimited by blank lines. Each event is buffered until its
// terminating blank line, then masked as a whole and flushed, so multi-line
// `data:` blocks that straddle network chunk boundaries are masked correctly
// while keeping the stream low-latency (events flush as they complete). Any
// trailing partial event is masked on EOF.
//
// inbound controls masking style: true for request bodies (full mask),
// false for responses (partial PII mask).
func (e *Engine) filterStream(w io.Writer, r io.Reader, rules []*Rule, inbound bool, model string) error {
	br := bufio.NewReader(r)
	event := &bytes.Buffer{}
	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			event.WriteString(line)
			if line == "\n" || line == "\r\n" {
				// blank line terminates an SSE event (or the leading blank)
				masked := e.Apply(rules, event.String(), inbound, model).Text
				if _, werr := io.WriteString(w, masked); werr != nil {
					return werr
				}
				event.Reset()
			}
		}
		if err != nil {
			if err == io.EOF {
				if event.Len() > 0 {
					masked := e.Apply(rules, event.String(), inbound, model).Text
					if _, werr := io.WriteString(w, masked); werr != nil {
						return werr
					}
				}
				return nil
			}
			return err
		}
	}
}

// streamMasker is an io.Writer that buffers an SSE byte stream and emits each
// complete event masked through the engine. It is used by the response writer
// for text/event-stream responses so masking never buffers the whole stream.
type streamMasker struct {
	engine  *Engine
	rules   []*Rule
	model   string
	out     io.Writer
	inbound bool

	buf bytes.Buffer
}

// Write buffers incoming bytes and emits any complete SSE events, masked.
func (s *streamMasker) Write(p []byte) (int, error) {
	n := len(p)
	s.buf.Write(p)
	if err := s.drain(); err != nil {
		return n, err
	}
	return n, nil
}

// drain emits every complete event currently buffered. It does not flush a
// partial trailing event, so masking boundaries stay aligned to event frames.
func (s *streamMasker) drain() error {
	b := s.buf.Bytes()
	for {
		end := s.nextEventEnd(b)
		if end < 0 {
			return nil
		}
		event := s.buf.Next(end)
		masked := s.engine.Apply(s.rules, string(event), s.inbound, s.model).Text
		if _, err := io.WriteString(s.out, masked); err != nil {
			return err
		}
		b = s.buf.Bytes()
	}
}

// nextEventEnd returns the byte index just past the blank line terminating the
// next SSE event in b, handling both LF and CRLF line endings, or -1 when no
// complete event is present.
func (s *streamMasker) nextEventEnd(b []byte) int {
	if i := bytes.Index(b, []byte("\n\n")); i >= 0 {
		return i + 2
	}
	if i := bytes.Index(b, []byte("\r\n\r\n")); i >= 0 {
		return i + 4
	}
	return -1
}

// Close flushes any remaining partial event (masked) and returns nil. It does
// not close the underlying writer.
func (s *streamMasker) Close() error {
	if s.buf.Len() > 0 {
		masked := s.engine.Apply(s.rules, s.buf.String(), s.inbound, s.model).Text
		if _, err := io.WriteString(s.out, masked); err != nil {
			return err
		}
		s.buf.Reset()
	}
	return nil
}

// FlushWriter is implemented by writers that support flushing partial output,
// used by the middleware to push masked SSE events promptly.
type FlushWriter interface {
	Flush() error
}
