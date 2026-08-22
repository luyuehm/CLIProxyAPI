package shadow

import (
	"sync"
	"time"
)

// EvalStore is a bounded, thread-safe in-memory ledger of shadow evaluation records.
// It retains the most recent N records and drops older ones on overflow.
// Optional TTL-based pruning removes records older than retainTTL.
// Optional max-records pruning keeps the store at or below the given count.
type EvalStore struct {
	mu        sync.RWMutex
	items     []*Record
	cap       int
	next      int // insertion index for ring-buffer mode
	full      bool
	retainTTL time.Duration // zero means no TTL pruning
	maxRecs   int           // zero means no count cap
}

// NewEvalStore creates a store with default capacity.
func NewEvalStore() *EvalStore {
	return NewEvalStoreWithCapacity(5000)
}

// NewEvalStoreWithCapacity creates a store with the given max record count.
func NewEvalStoreWithCapacity(cap int) *EvalStore {
	if cap <= 0 {
		cap = 5000
	}
	return &EvalStore{
		items: make([]*Record, cap),
		cap:   cap,
	}
}

// NewEvalStoreWithRetention creates a store that prunes records older than ttl.
func NewEvalStoreWithRetention(cap int, ttl time.Duration) *EvalStore {
	s := NewEvalStoreWithCapacity(cap)
	s.retainTTL = ttl
	return s
}

// SetRetention configures TTL-based pruning. A non-positive TTL disables it.
func (s *EvalStore) SetRetention(ttl time.Duration) {
	s.mu.Lock()
	s.retainTTL = ttl
	s.mu.Unlock()
}

// SetMaxRecords configures the max record count limit. A non-positive value disables it.
func (s *EvalStore) SetMaxRecords(max int) {
	s.mu.Lock()
	s.maxRecs = max
	s.mu.Unlock()
}

// Push inserts a record. Thread-safe and O(1). Older records are evicted when
// the store is full. Records older than retainTTL are pruned after insertion;
// max-records pruning reduces the count to at most maxRecs after insertion.
func (s *EvalStore) Push(rec *Record) {
	if rec == nil {
		return
	}
	if rec.ID == "" {
		rec.ID = shortHash([]byte(time.Now().String() + rec.PromptHash))
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.items[s.next] = rec
	s.next = (s.next + 1) % s.cap
	if s.next == 0 {
		s.full = true
	}

	// Prune after insertion so the newly-inserted record is also evaluated.
	if s.retainTTL > 0 {
		s.pruneExpiredLocked(time.Now().Add(-s.retainTTL))
	}
	if s.maxRecs > 0 {
		s.pruneToMaxRecordsLocked(s.maxRecs)
	}
}

// pruneExpiredLocked removes records older than the given cutoff, compacting
// the ring buffer in place. Caller must hold the write lock.
func (s *EvalStore) pruneExpiredLocked(cutoff time.Time) {
	count := s.count()
	if count == 0 {
		return
	}
	// Collect all valid records and filter expired ones.
	all := s.recentUnsafe(count) // newest first
	kept := make([]*Record, 0, len(all))
	for _, r := range all {
		if r != nil && !r.Timestamp.IsZero() && r.Timestamp.Before(cutoff) {
			continue // expired
		}
		kept = append(kept, r)
	}
	s.rebuild(kept)
}

// PruneToMaxRecords removes the oldest records until the store has at most
// maxRecords entries. No-op when maxRecords <= 0 or current count is already
// within the limit.
func (s *EvalStore) PruneToMaxRecords(maxRecords int) {
	if maxRecords <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneToMaxRecordsLocked(maxRecords)
}

// pruneToMaxRecordsLocked removes the oldest records while under the write lock.
func (s *EvalStore) pruneToMaxRecordsLocked(maxRecords int) {
	count := s.count()
	if count <= maxRecords {
		return
	}
	all := s.recentUnsafe(count) // newest first
	if len(all) <= maxRecords {
		return
	}
	kept := all[:maxRecords]
	s.rebuild(kept)
}

// rebuild resets the ring buffer from the given record slice (newest first, as
// returned by recentUnsafe). Caller must hold the write lock.
func (s *EvalStore) rebuild(records []*Record) {
	for i := range s.items {
		s.items[i] = nil
	}
	s.next = 0
	s.full = false
	// Reverse to insertion order (oldest first).
	for i := len(records) - 1; i >= 0; i-- {
		s.items[s.next] = records[i]
		s.next++
	}
	if s.next >= s.cap {
		s.full = true
		s.next = 0
	}
}

// Recent returns the most recent N records in reverse chronological order
// (newest first). When N <= 0 or N > cap, all stored records are returned.
func (s *EvalStore) Recent(n int) []*Record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.recentUnsafe(n)
}

// recentUnsafe returns recent records walking backwards from the last write
// cursor. The caller must hold at least a read lock.
func (s *EvalStore) recentUnsafe(n int) []*Record {
	count := s.count()
	if n <= 0 || n > count {
		n = count
	}
	if n <= 0 {
		return nil
	}

	// Last written index (newest record). When next == 0 (full, wrapped
	// exactly), the newest record sits at cap-1.
	start := s.next - 1
	if start < 0 {
		start = s.cap - 1
	}

	out := make([]*Record, 0, n)
	for i := 0; i < n; i++ {
		idx := start - i
		for idx < 0 {
			idx += s.cap
		}
		idx %= s.cap
		if s.items[idx] == nil {
			break
		}
		out = append(out, s.items[idx])
	}
	return out
}

// Query returns records matching the given model and candidate, most recent first.
func (s *EvalStore) Query(model, candidate string, limit int) []*Record {
	s.mu.RLock()
	defer s.mu.RUnlock()

	all := s.recentUnsafe(s.count())
	if len(all) == 0 {
		return nil
	}

	var filtered []*Record
	for _, r := range all {
		if r == nil {
			continue
		}
		if model != "" && r.Model != model {
			continue
		}
		if candidate != "" && r.Candidate != candidate {
			continue
		}
		filtered = append(filtered, r)
		if limit > 0 && len(filtered) >= limit {
			break
		}
	}
	return filtered
}

// Stats returns aggregate statistics for the given model pair.
func (s *EvalStore) Stats(model, candidate string) *EvalStats {
	records := s.Query(model, candidate, 0)
	if len(records) == 0 {
		return nil
	}

	stats := &EvalStats{
		TotalEvals:  len(records),
		ErrorCount:  0,
		TotalTokens: 0,
		TotalTTFTMs: 0,
		Similarity:  0,
	}

	var similaritySum float64
	var errorCount int
	for _, r := range records {
		if r.Error != "" {
			errorCount++
			continue
		}
		similaritySum += r.Similarity
		stats.TotalTokens += r.CandidateTokens
		stats.TotalTTFTMs += r.CandidateTTFT
	}

	validCount := len(records) - errorCount
	stats.ErrorCount = errorCount
	if validCount > 0 {
		stats.Similarity = similaritySum / float64(validCount)
		stats.AvgTTFTMs = stats.TotalTTFTMs / float64(validCount)
	}
	return stats
}

// Len returns the number of stored records.
func (s *EvalStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.count()
}

// Clear removes all records.
func (s *EvalStore) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = make([]*Record, s.cap)
	s.next = 0
	s.full = false
}

// prevIndex returns the i-th previous index in the ring buffer.
func (s *EvalStore) prevIndex(offset int) int {
	idx := s.next - offset
	for idx < 0 {
		idx += s.cap
	}
	return idx % s.cap
}

// count returns the number of records stored.
func (s *EvalStore) count() int {
	if s.full {
		return s.cap
	}
	return s.next
}

// EvalStats holds aggregate evaluation statistics.
type EvalStats struct {
	TotalEvals  int     `json:"total_evals"`
	ErrorCount  int     `json:"error_count"`
	TotalTokens int     `json:"total_tokens"`
	TotalTTFTMs float64 `json:"total_ttft_ms"`
	Similarity  float64 `json:"avg_similarity"`
	AvgTTFTMs   float64 `json:"avg_ttft_ms"`
}
