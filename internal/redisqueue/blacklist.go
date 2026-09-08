package redisqueue

import (
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// Stream registry and blacklist broadcast support for the in-process RESP bus.

const (
	// UsageStreamName is the Redis-stream key that mirrors the usage list.
	UsageStreamName = "usage:stream"
	// BlacklistChannel is the pub/sub channel for KEY_EXHAUSTED broadcast.
	BlacklistChannel = "blacklist"
	// KeyExhaustedEventType is the JSON "type" field for exhaustion broadcasts.
	KeyExhaustedEventType = "KEY_EXHAUSTED"
	// blacklistSubscriberBuffer is the buffered channel depth for subscribers.
	blacklistSubscriberBuffer = 128
)

// ExhaustedKeyEvent is the payload published on the blacklist channel when a key
// quota is exhausted. Consumers (the proxy's local gate) use it to update the
// in-memory blacklist with millisecond-level latency.
type ExhaustedKeyEvent struct {
	Type       string    `json:"type"`
	AuthIndex  string    `json:"auth_index"`
	Reason     string    `json:"reason,omitempty"`
	OccurredAt time.Time `json:"occurred_at"`
}

var (
	streamsMu sync.Mutex
	// streams maps a stream key to its in-memory stream.
	streams = make(map[string]*stream)

	blacklistMu     sync.Mutex
	blacklistChans  = make(map[uint64]chan []byte)
	blacklistNextID uint64
	blacklistStore  = make(map[string]struct{}) // auth_index -> blocked
)

// Stream returns (creating if needed) the in-memory stream for key.
func Stream(key string) *stream {
	streamsMu.Lock()
	defer streamsMu.Unlock()
	s, ok := streams[key]
	if !ok {
		s = newStream()
		streams[key] = s
	}
	return s
}

// DeleteStream removes a stream by key (used for tests / admin).
func DeleteStream(key string) {
	streamsMu.Lock()
	defer streamsMu.Unlock()
	delete(streams, key)
}

// SubscribeBlacklist registers a subscriber for the blacklist channel. Returns a
// receive channel and an unsubscribe func.
func SubscribeBlacklist() (<-chan []byte, func()) {
	blacklistMu.Lock()
	defer blacklistMu.Unlock()
	sub := make(chan []byte, blacklistSubscriberBuffer)
	blacklistNextID++
	id := blacklistNextID
	blacklistChans[id] = sub

	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			blacklistMu.Lock()
			defer blacklistMu.Unlock()
			if cur, ok := blacklistChans[id]; ok {
				delete(blacklistChans, id)
				close(cur)
			}
		})
	}
	return sub, unsubscribe
}

// PublishBlacklist broadcasts payload to all blacklist subscribers. It also
// records exhaustion into the local blacklist store so locally-published events
// are reflected immediately even without a subscriber.
func PublishBlacklist(payload []byte) {
	blacklistMu.Lock()
	defer blacklistMu.Unlock()

	// Record into the local block store for immediate gate enforcement.
	if event, ok := parseExhaustedEvent(payload); ok {
		blacklistStore[event.AuthIndex] = struct{}{}
	}

	for id, sub := range blacklistChans {
		cloned := append([]byte(nil), payload...)
		select {
		case sub <- cloned:
		default:
			// Slow subscriber: drop it rather than block the publisher.
			delete(blacklistChans, id)
			close(sub)
		}
	}
}

// BlacklistSubscriberCount returns the number of active blacklist subscribers.
func BlacklistSubscriberCount() int {
	blacklistMu.Lock()
	defer blacklistMu.Unlock()
	return len(blacklistChans)
}

// IsKeyBlocked reports whether authIndex is currently in the blacklist store.
func IsKeyBlocked(authIndex string) bool {
	if authIndex == "" {
		return false
	}
	blacklistMu.Lock()
	defer blacklistMu.Unlock()
	_, blocked := blacklistStore[authIndex]
	return blocked
}

// ClearBlacklist removes an authIndex from the local blacklist store. It also
// broadcasts an unblock event so other nodes clear their local copies.
func ClearBlacklist(authIndex string) {
	blacklistMu.Lock()
	if _, ok := blacklistStore[authIndex]; ok {
		delete(blacklistStore, authIndex)
	}
	blacklistMu.Unlock()

	event := ExhaustedKeyEvent{
		Type:       "KEY_REVOKED",
		AuthIndex:  authIndex,
		OccurredAt: time.Now(),
	}
	if payload, err := json.Marshal(event); err == nil {
		PublishBlacklist(payload)
	}
}

// BlacklistSnapshot returns a copy of currently blocked auth indexes.
func BlacklistSnapshot() []string {
	blacklistMu.Lock()
	defer blacklistMu.Unlock()
	out := make([]string, 0, len(blacklistStore))
	for idx := range blacklistStore {
		out = append(out, idx)
	}
	return out
}

func parseExhaustedEvent(payload []byte) (ExhaustedKeyEvent, bool) {
	var event ExhaustedKeyEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		return event, false
	}
	if event.Type != KeyExhaustedEventType {
		return event, false
	}
	if strings.TrimSpace(event.AuthIndex) == "" {
		return event, false
	}
	return event, true
}
