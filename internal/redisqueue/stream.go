package redisqueue

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Stream entry / consumer-group support for the in-process Redis RESP emulator.
//
// The proxy's management Redis bus is an in-memory RESP server. The Keeper
// metering service consumes usage through it. These helpers add a
// Redis-Streams-compatible surface (XADD / XGROUP / XREADGROUP / XACK) on top of
// the same in-memory store, so the Keeper can use consumer-group semantics for
// concurrent, at-least-once consumption of usage records without a real Redis.

// ErrStreamUnsupported is returned when a command targets a key that is not a
// stream (for example LPOP keys that are not streams).
var ErrStreamUnsupported = errors.New("stream key is not a stream")

// StreamEntry is a single appended record in a stream.
type StreamEntry struct {
	ID       string
	Fields   map[string]string
	enqueued time.Time
}

// streamGroup is a consumer group over a stream.
type streamGroup struct {
	name       string
	nextOffset int              // index into stream.entries (last delivered+1)
	pending    map[string][]int // consumer -> indexes of delivered-but-unacked entries
	consumers  map[string]struct{}
}

type stream struct {
	mu      sync.Mutex
	entries []StreamEntry
	groups  map[string]*streamGroup
}

func newStream() *stream {
	return &stream{groups: make(map[string]*streamGroup)}
}

func nextStreamID(now time.Time, lastID string) string {
	// Use <millis>-<seq> to mirror Redis stream IDs. A caller-supplied explicit
	// ID (for example "0" sentinel in tests) is preserved by the protocol layer.
	ms := now.UnixMilli()
	seq := int64(0)
	if lastID != "" {
		if last, _, ok := strings.Cut(lastID, "-"); ok {
			if lastMS, err := strconv.ParseInt(last, 10, 64); err == nil && lastMS == ms {
				seq = 1
			}
		}
	}
	return fmt.Sprintf("%d-%d", ms, seq)
}

// Append appends an entry and returns its generated ID. The caller supplies the
// fields; the ID is monotonic in wall-clock millis.
func (s *stream) Append(now time.Time, fields map[string]string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := nextStreamID(now, s.lastIDLocked())
	s.entries = append(s.entries, StreamEntry{ID: id, Fields: fields, enqueued: now})
	return id
}

func (s *stream) lastIDLocked() string {
	if len(s.entries) == 0 {
		return ""
	}
	return s.entries[len(s.entries)-1].ID
}

// Len returns the number of entries in the stream.
func (s *stream) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// PendingCount returns the number of pending (delivered but unacked) entries in
// a group regardless of consumer. Used for XACK bookkeeping.
func (s *stream) PendingCount(group string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.groups[group]
	if !ok {
		return 0
	}
	var total int
	for _, indexes := range g.pending {
		total += len(indexes)
	}
	return total
}

// CreateGroup creates a consumer group with an initial offset. mkstream is true
// when the caller passed MKSTREAM; an existing group is an error unless
// mkstream allows it.
func (s *stream) CreateGroup(name, startID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.groups[name]; exists {
		return fmt.Errorf("BUSYGROUP Consumer Group name already exists")
	}
	offset := len(s.entries)
	if startID == "" || startID == "0" || startID == "0-0" {
		offset = 0
	} else if startID == "$" {
		offset = len(s.entries)
	}
	g := &streamGroup{
		name:       name,
		nextOffset: offset,
		pending:    make(map[string][]int),
		consumers:  make(map[string]struct{}),
	}
	s.groups[name] = g
	return nil
}

// ReadGroup returns up to count new entries for consumer in group, advancing the
// group offset. It returns entries not yet acknowledged (first call delivers
// them; subsequent calls with the same offset deliver pending unacked entries).
func (s *stream) ReadGroup(group, consumer string, count int) []StreamEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.groups[group]
	if !ok {
		return nil
	}
	g.consumers[consumer] = struct{}{}

	// New deliveries: everything after nextOffset, up to count.
	var out []StreamEntry
	idx := g.nextOffset
	for idx < len(s.entries) && len(out) < count {
		entry := s.entries[idx]
		out = append(out, entry)
		g.pending[consumer] = append(g.pending[consumer], idx)
		idx++
	}
	g.nextOffset = idx

	// If we have room and the consumer already has pending unacked entries,
	// re-deliver the oldest pending ones first (at-least-once semantics).
	if len(out) < count {
		for _, pendingIdx := range g.pending[consumer] {
			if pendingIdx >= len(s.entries) {
				continue
			}
			entry := s.entries[pendingIdx]
			dup := false
			for _, o := range out {
				if o.ID == entry.ID {
					dup = true
					break
				}
			}
			if !dup {
				out = append(out, entry)
			}
			if len(out) >= count {
				break
			}
		}
	}
	return out
}

// Ack marks entries (by ID) as processed for a consumer in group, removing them
// from the pending set. It follows Redis XACK semantics: acknowledgement is
// per-group (any consumer may acknowledge any pending entry, irrespective of
// which consumer originally received it).
func (s *stream) Ack(group string, ids []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.groups[group]
	if !ok {
		return
	}
	byID := make(map[string]int, len(ids))
	for _, id := range ids {
		byID[id] = 1
	}
	for consumer, pending := range g.pending {
		if len(pending) == 0 {
			continue
		}
		kept := pending[:0]
		for _, idx := range pending {
			if idx >= len(s.entries) {
				continue
			}
			if _, ack := byID[s.entries[idx].ID]; ack {
				continue
			}
			kept = append(kept, idx)
		}
		g.pending[consumer] = kept
	}
	// Drop empty pending lists so PendingCount reflects reality.
	for consumer := range g.pending {
		if len(g.pending[consumer]) == 0 {
			delete(g.pending, consumer)
		}
	}
}

// TrimByID drops all entries at or before the given ID. Used for stream
// maintenance so usage streams do not grow without bound.
func (s *stream) TrimByID(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.entries) == 0 {
		return
	}
	keepFrom := len(s.entries)
	for i, entry := range s.entries {
		if entry.ID > id {
			keepFrom = i
			break
		}
	}
	if keepFrom > 0 {
		s.entries = append([]StreamEntry(nil), s.entries[keepFrom:]...)
		// Rebase group offsets and pending indexes.
		for _, g := range s.groups {
			if g.nextOffset > keepFrom {
				g.nextOffset -= keepFrom
			} else {
				g.nextOffset = 0
			}
			for consumer, indexes := range g.pending {
				rebased := make([]int, 0, len(indexes))
				for _, idx := range indexes {
					if idx >= keepFrom {
						rebased = append(rebased, idx-keepFrom)
					}
				}
				g.pending[consumer] = rebased
			}
		}
	}
}

// KeyExists reports whether a stream was ever created/appended under a name.
func (s *stream) Empty() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries) == 0 && len(s.groups) == 0
}
