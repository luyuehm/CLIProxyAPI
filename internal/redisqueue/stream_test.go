package redisqueue

import (
	"encoding/json"
	"testing"
	"time"
)

func TestStreamAppendAndReadGroup(t *testing.T) {
	st := newStream()
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	id1 := st.Append(now, map[string]string{"payload": "a"})
	id2 := st.Append(now.Add(time.Millisecond), map[string]string{"payload": "b"})
	if id1 == id2 {
		t.Fatalf("expected distinct stream ids, got %q and %q", id1, id2)
	}
	if st.Len() != 2 {
		t.Fatalf("expected len 2, got %d", st.Len())
	}

	if err := st.CreateGroup("keeper", "0"); err != nil {
		t.Fatalf("create group: %v", err)
	}
	if err := st.CreateGroup("keeper", "0"); err == nil {
		t.Fatal("expected duplicate group error")
	}

	entries := st.ReadGroup("keeper", "worker-1", 10)
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].ID != id1 || entries[1].ID != id2 {
		t.Fatalf("unexpected order: %q %q", entries[0].ID, entries[1].ID)
	}
	if entries[0].Fields["payload"] != "a" {
		t.Fatalf("unexpected payload: %v", entries[0].Fields)
	}

	// Second read with no new entries re-delivers pending unacked.
	more := st.ReadGroup("keeper", "worker-1", 10)
	if len(more) != 2 {
		t.Fatalf("expected 2 redelivered pending, got %d", len(more))
	}

	// A second consumer sees only NEW entries (the group offset is shared). Since
	// worker-1 already drained the stream, worker-2 sees nothing new.
	more2 := st.ReadGroup("keeper", "worker-2", 10)
	if len(more2) != 0 {
		t.Fatalf("expected consumer 2 to see no new entries, got %d", len(more2))
	}

	// Ack clears pending group-wide (Redis XACK semantics).
	st.Ack("keeper", []string{id1, id2})
	if got := st.PendingCount("keeper"); got != 0 {
		t.Fatalf("expected 0 pending after ack, got %d", got)
	}
}

func TestStreamTrimByIDRebases(t *testing.T) {
	st := newStream()
	now := time.Now()
	_ = st.Append(now, map[string]string{"payload": "1"})
	id2 := st.Append(now.Add(time.Millisecond), map[string]string{"payload": "2"})
	_ = st.Append(now.Add(2*time.Millisecond), map[string]string{"payload": "3"})
	_ = st.CreateGroup("g", "0")
	st.ReadGroup("g", "c", 10)

	st.TrimByID(id2)
	if st.Len() != 1 {
		t.Fatalf("expected 1 entry after trim, got %d", st.Len())
	}
	entries := st.ReadGroup("g", "c", 10)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry after trim+read, got %d", len(entries))
	}
	if entries[0].Fields["payload"] != "3" {
		t.Fatalf("expected payload 3, got %v", entries[0].Fields)
	}
}

func TestBlacklistPublishAndGate(t *testing.T) {
	// Ensure a clean store.
	blacklistMu.Lock()
	blacklistStore = make(map[string]struct{})
	blacklistMu.Unlock()

	sub, unsubscribe := SubscribeBlacklist()
	defer unsubscribe()

	payload, err := json.Marshal(ExhaustedKeyEvent{
		Type:       KeyExhaustedEventType,
		AuthIndex:  "claude|alice",
		Reason:     "monthly limit",
		OccurredAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	PublishBlacklist(payload)
	select {
	case got := <-sub:
		var ev ExhaustedKeyEvent
		if err := json.Unmarshal(got, &ev); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if ev.Type != KeyExhaustedEventType || ev.AuthIndex != "claude|alice" {
			t.Fatalf("unexpected event: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for blacklist event")
	}

	if !IsKeyBlocked("claude|alice") {
		t.Fatal("expected key to be blocked after publish")
	}
	if IsKeyBlocked("claude|bob") {
		t.Fatal("expected unrelated key not to be blocked")
	}

	ClearBlacklist("claude|alice")
	if IsKeyBlocked("claude|alice") {
		t.Fatal("expected key to be unblocked after clear")
	}
}

func TestEnqueueMirrorsToStream(t *testing.T) {
	SetEnabled(true)
	defer SetEnabled(false)
	DeleteStream(UsageStreamName)

	Enqueue([]byte(`{"request_id":"x1"}`))
	st := Stream(UsageStreamName)
	if st.Len() != 1 {
		t.Fatalf("expected 1 stream entry, got %d", st.Len())
	}
	if err := st.CreateGroup("keeper", "0"); err != nil {
		t.Fatalf("create group: %v", err)
	}
	entries := st.ReadGroup("keeper", "c", 10)
	if len(entries) != 1 {
		t.Fatalf("expected 1 group entry, got %d", len(entries))
	}
	if entries[0].Fields["payload"] != `{"request_id":"x1"}` {
		t.Fatalf("unexpected payload: %v", entries[0].Fields)
	}
}
