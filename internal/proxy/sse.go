package proxy

import (
	"strconv"
	"strings"
)

// SSEFrameSeparator terminates a Server-Sent Events frame.
const SSEFrameSeparator = "\n\n"

// SplitSSEFrames splits raw SSE bytes into complete frames and a residual tail.
// Every returned frame keeps its terminating blank line so it can be replayed
// verbatim. Bytes that do not yet form a complete frame are returned as rest.
//
// Both "\n\n" and "\r\n\r\n" terminators are recognised.
func SplitSSEFrames(p []byte) (frames [][]byte, rest []byte) {
	start := 0
	for {
		end := nextFrameEnd(p, start)
		if end < 0 {
			break
		}
		frames = append(frames, p[start:end])
		start = end
	}
	return frames, p[start:]
}

// nextFrameEnd returns the offset just past the first frame terminator at or
// after from, or -1 when no complete frame is present.
func nextFrameEnd(p []byte, from int) int {
	for i := from; i < len(p); i++ {
		if p[i] != '\n' && p[i] != '\r' {
			continue
		}
		// "\n\n"
		if i+1 < len(p) && p[i] == '\n' && p[i+1] == '\n' {
			return i + 2
		}
		// "\r\n\r\n"
		if i+3 < len(p) && p[i] == '\r' && p[i+1] == '\n' && p[i+2] == '\r' && p[i+3] == '\n' {
			return i + 4
		}
	}
	return -1
}

// WithEventID returns frame with a leading "id: <seq>" field so SSE clients can
// echo the last processed position through Last-Event-ID. Any id field the
// upstream already emitted is dropped first: the gateway assigns the
// authoritative monotonic sequence, and the guarantee that a resumed stream
// replays exactly the missing frames depends on that. Comment-only frames
// (keep-alives) are returned unchanged.
func WithEventID(frame []byte, seq int64) []byte {
	if seq <= 0 || len(frame) == 0 {
		return frame
	}
	if isCommentFrame(frame) {
		return frame
	}
	out := make([]byte, 0, len(frame)+24)
	out = append(out, "id: "...)
	out = strconv.AppendInt(out, seq, 10)
	out = append(out, '\n')

	for _, line := range splitLines(frame) {
		if isIDField(line) {
			continue
		}
		out = append(out, line...)
	}
	return out
}

// isCommentFrame reports whether the frame carries only SSE comments.
func isCommentFrame(frame []byte) bool {
	for _, line := range splitLines(frame) {
		trimmed := strings.TrimRight(string(line), "\r\n")
		if trimmed == "" {
			continue
		}
		return strings.HasPrefix(trimmed, ":")
	}
	return true
}

// isIDField reports whether line is an SSE id field.
func isIDField(line []byte) bool {
	return strings.HasPrefix(strings.TrimRight(string(line), "\r\n"), "id:")
}

// splitLines splits b on '\n', keeping the line terminators.
func splitLines(b []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i := 0; i < len(b); i++ {
		if b[i] == '\n' {
			lines = append(lines, b[start:i+1])
			start = i + 1
		}
	}
	if start < len(b) {
		lines = append(lines, b[start:])
	}
	return lines
}

// ParseEventID parses a Last-Event-ID value into a sequence number.
func ParseEventID(raw string) (int64, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, false
	}
	seq, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil || seq < 0 {
		return 0, false
	}
	return seq, true
}
