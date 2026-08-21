package stream

import (
	"sync"
)

// MaxBufferBytes is the default maximum bytes to buffer for breakpoint resume.
const MaxBufferBytes = 256 * 1024 // 256KB

// BufferChunk represents a single chunk in the sliding window buffer.
type BufferChunk struct {
	// Payload is the raw chunk bytes (a whole SSE frame for the proxy layer).
	Payload []byte
	// Seq is an optional monotonic sequence number assigned by the writer.
	// Zero means unsequenced.
	Seq int64
}

// StreamBuffer implements a sliding-window circular buffer for streaming chunks.
// It stores the most recent chunks up to a configurable byte limit, enabling
// transparent breakpoint resume after downstream disconnection.
type StreamBuffer struct {
	mu       sync.RWMutex
	window   []BufferChunk
	start    int
	count    int
	maxBytes int
	curBytes int
}

// NewStreamBuffer creates a new StreamBuffer with the given max byte capacity.
// Pass 0 to use the default (256KB).
func NewStreamBuffer(maxBytes int) *StreamBuffer {
	if maxBytes <= 0 {
		maxBytes = MaxBufferBytes
	}
	return &StreamBuffer{
		window:   make([]BufferChunk, 64),
		maxBytes: maxBytes,
	}
}

// Write adds a chunk to the buffer, evicting the oldest chunks if the total
// exceeds maxBytes. Each payload is deep-copied so the caller retains ownership.
func (b *StreamBuffer) Write(payload []byte) {
	b.WriteSeq(payload, 0)
}

// WriteSeq adds a sequenced chunk to the buffer.
func (b *StreamBuffer) WriteSeq(payload []byte, seq int64) {
	if len(payload) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.count == len(b.window) {
		b.grow()
	}

	chunk := BufferChunk{Payload: append([]byte(nil), payload...), Seq: seq}
	idx := (b.start + b.count) % len(b.window)
	b.window[idx] = chunk
	b.count++
	b.curBytes += len(payload)

	for b.curBytes > b.maxBytes && b.count > 1 {
		oldest := b.window[b.start]
		b.window[b.start] = BufferChunk{}
		b.start = (b.start + 1) % len(b.window)
		b.count--
		b.curBytes -= len(oldest.Payload)
	}
}

// Snapshot returns a deep copy of all buffered chunks in insertion order.
// Returns nil when the buffer is empty.
func (b *StreamBuffer) Snapshot() []BufferChunk {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.count == 0 {
		return nil
	}
	result := make([]BufferChunk, b.count)
	for i := 0; i < b.count; i++ {
		idx := (b.start + i) % len(b.window)
		result[i] = BufferChunk{
			Payload: append([]byte(nil), b.window[idx].Payload...),
			Seq:     b.window[idx].Seq,
		}
	}
	return result
}

// Size returns the current byte count in the buffer.
func (b *StreamBuffer) Size() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.curBytes
}

// Len returns the number of chunks in the buffer.
func (b *StreamBuffer) Len() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.count
}

// Reset clears the buffer and releases memory.
func (b *StreamBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.window = make([]BufferChunk, 64)
	b.start = 0
	b.count = 0
	b.curBytes = 0
}

func (b *StreamBuffer) grow() {
	n := len(b.window) * 2
	newWindow := make([]BufferChunk, n)
	for i := 0; i < b.count; i++ {
		idx := (b.start + i) % len(b.window)
		newWindow[i] = b.window[idx]
	}
	b.window = newWindow
	b.start = 0
}
