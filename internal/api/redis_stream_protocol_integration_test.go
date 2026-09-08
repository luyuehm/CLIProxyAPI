package api

import (
	"bufio"
	"net"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/redisqueue"
)

// readStreamReadResult reads the XREADGROUP response:
//
//	*1 *2 $key *N ($id *2 $field $value ...) ...
//
// Returns the stream key and per-entry IDs with their field/value pairs.
func readStreamReadResult(t *testing.T, r *bufio.Reader) (key string, entries map[string]map[string]string) {
	t.Helper()

	// Top-level *1
	topCount, err := readRESPArrayLength(r)
	if err != nil {
		t.Fatalf("read stream top array: %v", err)
	}
	if topCount != 1 {
		t.Fatalf("expected top array length 1, got %d", topCount)
	}
	// *2 $key *N
	streamArray, err := readRESPArrayLength(r)
	if err != nil {
		t.Fatalf("read stream array: %v", err)
	}
	if streamArray != 2 {
		t.Fatalf("expected stream array length 2, got %d", streamArray)
	}
	keyBytes, err := readTestRESPBulkString(r)
	if err != nil {
		t.Fatalf("read stream key: %v", err)
	}
	key = string(keyBytes)

	entryCount, err := readRESPArrayLength(r)
	if err != nil {
		t.Fatalf("read entries array: %v", err)
	}
	entries = make(map[string]map[string]string, entryCount)
	for i := 0; i < entryCount; i++ {
		// *2 $id $fields
		if err := expectRESPArrayLength(r, 2); err != nil {
			t.Fatalf("entry array: %v", err)
		}
		idBytes, err := readTestRESPBulkString(r)
		if err != nil {
			t.Fatalf("read entry id: %v", err)
		}
		fieldsCount, err := readRESPArrayLength(r)
		if err != nil {
			t.Fatalf("read fields array: %v", err)
		}
		fields := make(map[string]string, fieldsCount/2)
		for j := 0; j < fieldsCount; j += 2 {
			field, err := readTestRESPBulkString(r)
			if err != nil {
				t.Fatalf("read field: %v", err)
			}
			value, err := readTestRESPBulkString(r)
			if err != nil {
				t.Fatalf("read value: %v", err)
			}
			fields[string(field)] = string(value)
		}
		entries[string(idBytes)] = fields
	}
	return key, entries
}

func readRESPArrayLength(r *bufio.Reader) (int, error) {
	prefix, err := r.ReadByte()
	if err != nil {
		return 0, err
	}
	if prefix != '*' {
		return 0, errUnexpectedPrefix(prefix)
	}
	line, err := readTestRESPLine(r)
	if err != nil {
		return 0, err
	}
	return strconvAtoiTest(line)
}

func expectRESPArrayLength(r *bufio.Reader, want int) error {
	got, err := readRESPArrayLength(r)
	if err != nil {
		return err
	}
	if got != want {
		return errUnexpectedArrayLength(got, want)
	}
	return nil
}

type testProtocolError string

func (e testProtocolError) Error() string { return string(e) }

func errUnexpectedPrefix(prefix byte) error {
	return testProtocolError("unexpected RESP prefix " + string(prefix))
}

func errUnexpectedArrayLength(got, want int) error {
	return testProtocolError("unexpected array length")
}

func strconvAtoiTest(s string) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, testProtocolError("not an integer")
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

func TestRedisProtocol_StreamsAndBlacklistEndToEnd(t *testing.T) {
	const managementPassword = "test-stream-password"

	t.Setenv("MANAGEMENT_PASSWORD", managementPassword)
	redisqueue.SetEnabled(true)
	redisqueue.DeleteStream(redisqueue.UsageStreamName)
	t.Cleanup(func() { redisqueue.SetEnabled(false) })

	// Clear blacklist store.
	for _, idx := range redisqueue.BlacklistSnapshot() {
		redisqueue.ClearBlacklist(idx)
	}

	server := newTestServer(t)
	if !server.managementRoutesEnabled.Load() {
		t.Fatalf("expected managementRoutesEnabled to be true")
	}

	addr, stop := startRedisMuxListener(t, server)
	t.Cleanup(stop)

	conn, errDial := net.DialTimeout("tcp", addr, time.Second)
	if errDial != nil {
		t.Fatalf("failed to dial redis listener: %v", errDial)
	}
	t.Cleanup(func() { _ = conn.Close() })

	reader := bufio.NewReader(conn)
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	// AUTH.
	if errWrite := writeTestRESPCommand(conn, "AUTH", managementPassword); errWrite != nil {
		t.Fatalf("AUTH write: %v", errWrite)
	}
	if msg, err := readTestRESPSimpleString(reader); err != nil || msg != "OK" {
		t.Fatalf("AUTH response: %q err=%v", msg, err)
	}

	// XGROUP CREATE usage:stream keeper 0 MKSTREAM
	if err := writeTestRESPCommand(conn, "XGROUP", "CREATE", "usage:stream", "keeper", "0", "MKSTREAM"); err != nil {
		t.Fatalf("XGROUP write: %v", err)
	}
	if msg, err := readTestRESPSimpleString(reader); err != nil || msg != "OK" {
		t.Fatalf("XGROUP response: %q err=%v", msg, err)
	}

	// XADD usage:stream * payload {"request_id":"x1"}
	if err := writeTestRESPCommand(conn, "XADD", "usage:stream", "*", "payload", `{"request_id":"x1"}`); err != nil {
		t.Fatalf("XADD write: %v", err)
	}
	id1, err := readTestRESPBulkString(reader)
	if err != nil || len(id1) == 0 {
		t.Fatalf("XADD id: %q err=%v", string(id1), err)
	}
	if err := writeTestRESPCommand(conn, "XADD", "usage:stream", "*", "payload", `{"request_id":"x2"}`); err != nil {
		t.Fatalf("XADD write: %v", err)
	}
	id2, err := readTestRESPBulkString(reader)
	if err != nil || len(id2) == 0 {
		t.Fatalf("XADD id2: %q err=%v", string(id2), err)
	}

	// XLEN.
	if err := writeTestRESPCommand(conn, "XLEN", "usage:stream"); err != nil {
		t.Fatalf("XLEN write: %v", err)
	}
	if line, err := readTestRESPLine(reader); err != nil || line != ":2" {
		t.Fatalf("XLEN response: %q err=%v", line, err)
	}

	// XREADGROUP GROUP keeper w1 COUNT 10 BLOCK 200 STREAMS usage:stream >
	args := []string{"XREADGROUP", "GROUP", "keeper", "w1", "COUNT", "10", "BLOCK", "200", "STREAMS", "usage:stream", ">"}
	if err := writeTestRESPCommand(conn, args...); err != nil {
		t.Fatalf("XREADGROUP write: %v", err)
	}
	key, entries := readStreamReadResult(t, reader)
	if key != "usage:stream" {
		t.Fatalf("stream key: %q", key)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 stream entries, got %d", len(entries))
	}
	var firstID string
	for id, fields := range entries {
		if fields["payload"] != `{"request_id":"x1"}` && fields["payload"] != `{"request_id":"x2"}` {
			t.Fatalf("unexpected entry payload: %v", fields)
		}
		if firstID == "" {
			firstID = id
		}
	}
	if string(id1) != firstID && string(id2) != firstID {
		t.Fatalf("first id %q not one of XADD ids", firstID)
	}

	// XACK usage:stream keeper <id1> <id2>
	if err := writeTestRESPCommand(conn, "XACK", "usage:stream", "keeper", string(id1), string(id2)); err != nil {
		t.Fatalf("XACK write: %v", err)
	}
	if line, err := readTestRESPLine(reader); err != nil || line != ":2" {
		t.Fatalf("XACK response: %q err=%v", line, err)
	}

	// BLACKLIST query before publish -> 0
	if err := writeTestRESPCommand(conn, "BLACKLIST", "claude|alice"); err != nil {
		t.Fatalf("BLACKLIST write: %v", err)
	}
	if line, err := readTestRESPLine(reader); err != nil || line != ":0" {
		t.Fatalf("BLACKLIST before publish: %q err=%v", line, err)
	}

	// PUBLISH blacklist KEY_EXHAUSTED
	event := `{"type":"KEY_EXHAUSTED","auth_index":"claude|alice","reason":"monthly","occurred_at":"2026-09-08T00:00:00Z"}`
	if err := writeTestRESPCommand(conn, "PUBLISH", "blacklist", event); err != nil {
		t.Fatalf("PUBLISH write: %v", err)
	}
	// Integer reply (subscriber count).
	if line, err := readTestRESPLine(reader); err != nil || line[0] != ':' {
		t.Fatalf("PUBLISH response: %q err=%v", line, err)
	}

	// BLACKLIST query after publish -> 1
	if err := writeTestRESPCommand(conn, "BLACKLIST", "claude|alice"); err != nil {
		t.Fatalf("BLACKLIST write: %v", err)
	}
	if line, err := readTestRESPLine(reader); err != nil || line != ":1" {
		t.Fatalf("BLACKLIST after publish: %q err=%v", line, err)
	}

	if !redisqueue.IsKeyBlocked("claude|alice") {
		t.Fatal("expected claude|alice to be blocked")
	}
	if redisqueue.IsKeyBlocked("claude|bob") {
		t.Fatal("expected claude|bob not to be blocked")
	}
}
