package helper

import (
	"bytes"
	"testing"
)

func TestCSVUTF8BOM(t *testing.T) {
	if len(CSVUTF8BOM) != 3 {
		t.Fatalf("expected 3-byte UTF-8 BOM, got %d bytes: %v", len(CSVUTF8BOM), CSVUTF8BOM)
	}
	want := []byte{0xEF, 0xBB, 0xBF}
	if !bytes.Equal(CSVUTF8BOM, want) {
		t.Fatalf("expected BOM %v, got %v", want, CSVUTF8BOM)
	}
}
