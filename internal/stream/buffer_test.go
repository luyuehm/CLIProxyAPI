package stream

import (
	"testing"
)

func TestNewStreamBuffer(t *testing.T) {
	b := NewStreamBuffer(0)
	if b.maxBytes != MaxBufferBytes {
		t.Fatalf("expected maxBytes %d, got %d", MaxBufferBytes, b.maxBytes)
	}
	if b.Len() != 0 {
		t.Fatalf("expected empty buffer, got len %d", b.Len())
	}
}

func TestStreamBufferWriteAndSnapshot(t *testing.T) {
	b := NewStreamBuffer(100)
	b.Write([]byte("hello"))
	b.Write([]byte("world"))

	snap := b.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(snap))
	}
	if string(snap[0].Payload) != "hello" {
		t.Fatalf("expected 'hello', got %q", string(snap[0].Payload))
	}
	if string(snap[1].Payload) != "world" {
		t.Fatalf("expected 'world', got %q", string(snap[1].Payload))
	}
}

func TestStreamBufferZeroPayload(t *testing.T) {
	b := NewStreamBuffer(100)
	b.Write(nil)
	b.Write([]byte{})
	if b.Len() != 0 {
		t.Fatalf("expected empty buffer after zero-length writes, got len %d", b.Len())
	}
}

func TestStreamBufferEviction(t *testing.T) {
	b := NewStreamBuffer(10)
	b.Write([]byte("aaaaa")) // 5
	b.Write([]byte("bbbbb")) // 5, total 10
	b.Write([]byte("ccccc")) // 5, should evict "aaaaa"

	if b.Len() != 2 {
		t.Fatalf("expected 2 chunks, got %d", b.Len())
	}
	snap := b.Snapshot()
	if string(snap[0].Payload) != "bbbbb" {
		t.Fatalf("expected 'bbbbb' as oldest, got %q", string(snap[0].Payload))
	}
	if string(snap[1].Payload) != "ccccc" {
		t.Fatalf("expected 'ccccc' as newest, got %q", string(snap[1].Payload))
	}
}

func TestStreamBufferEvictionWithLargeChunk(t *testing.T) {
	b := NewStreamBuffer(10)
	// a single chunk over maxBytes is kept (at least one chunk is always retained)
	b.Write([]byte("this is more than ten bytes"))
	if b.Len() != 1 {
		t.Fatalf("expected 1 chunk (oversized), got %d", b.Len())
	}
	snap := b.Snapshot()
	if string(snap[0].Payload) != "this is more than ten bytes" {
		t.Fatalf("oversized chunk payload mismatch")
	}
}

func TestStreamBufferSnapshotReturnsCopy(t *testing.T) {
	b := NewStreamBuffer(100)
	original := []byte("test-data")
	b.Write(original)
	// Modify original after write
	original[0] = 'X'

	snap := b.Snapshot()
	if string(snap[0].Payload) == "Xest-data" {
		t.Fatal("snapshot should be a deep copy, not alias the input")
	}
	if string(snap[0].Payload) != "test-data" {
		t.Fatalf("unexpected snapshot content: %q", string(snap[0].Payload))
	}
}

func TestStreamBufferReset(t *testing.T) {
	b := NewStreamBuffer(100)
	b.Write([]byte("data"))
	b.Reset()
	if b.Len() != 0 {
		t.Fatalf("expected empty after reset, got len %d", b.Len())
	}
	if b.Size() != 0 {
		t.Fatalf("expected 0 bytes after reset, got %d", b.Size())
	}
}

func TestStreamBufferWriteSeq(t *testing.T) {
	b := NewStreamBuffer(100)
	b.WriteSeq([]byte("first"), 1)
	b.WriteSeq([]byte("second"), 2)

	snap := b.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(snap))
	}
	if snap[0].Seq != 1 || snap[1].Seq != 2 {
		t.Fatalf("sequence numbers mismatch: got %d, %d", snap[0].Seq, snap[1].Seq)
	}
}

func TestStreamBufferConcurrentSafety(t *testing.T) {
	b := NewStreamBuffer(1024)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			b.Write([]byte("concurrent"))
		}
		close(done)
	}()
	for i := 0; i < 100; i++ {
		_ = b.Snapshot()
		_ = b.Size()
		_ = b.Len()
	}
	<-done
	if b.Len() == 0 {
		t.Fatal("buffer should not be empty after concurrent writes")
	}
}

func TestStreamBufferLargeCapacity(t *testing.T) {
	b := NewStreamBuffer(50_000)
	total := 0
	for i := 0; i < 1000; i++ {
		chunk := make([]byte, 100)
		for j := range chunk {
			chunk[j] = byte(i)
		}
		b.Write(chunk)
		total += 100
	}
	// Should have evicted to within maxBytes
	if b.Size() > 50_000 {
		t.Fatalf("buffer size %d exceeded max 50000", b.Size())
	}
	if b.Len() == 0 {
		t.Fatal("buffer should not be empty")
	}
}

func TestStreamBufferEmptySnapshot(t *testing.T) {
	b := NewStreamBuffer(100)
	snap := b.Snapshot()
	if snap != nil {
		t.Fatal("expected nil snapshot for empty buffer")
	}
}
