package shadow

import (
	"sync"
	"time"
)

// EvalStore is a bounded, thread-safe in-memory ledger of shadow evaluation records.
// It retains the most recent N records and drops older ones on overflow.
type EvalStore struct {
	mu    sync.RWMutex
	items []*Record
	cap   int
	next  int // insertion index for ring-buffer mode
	full  bool
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

// Push inserts a record. Thread-safe and O(1). Older records are evicted when
// the store is full.
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
}

// Recent returns the most recent N records in reverse chronological order.
// When N <= 0 or N > cap, all stored records are returned.
func (s *EvalStore) Recent(n int) []*Record {
	s.mu.RLock()
	defer s.mu.RUnlock()

	count := s.count()
	if n <= 0 || n > count {
		n = count
	}
	if n <= 0 {
		return nil
	}

	out := make([]*Record, 0, n)
	idx := s.prevIndex(1)
	for i := 0; i < n; i++ {
		if s.items[idx] == nil {
			break
		}
		out = append(out, s.items[idx])
		idx = s.prevIndex(s.cap - (n - i - 1))
		// simpler: walk backwards
	}
	return s.recentUnsafe(n)
}

// recentUnsafe returns recent records walking backwards from the write cursor.
func (s *EvalStore) recentUnsafe(n int) []*Record {
	count := s.count()
	if n > count {
		n = count
	}
	if n <= 0 {
		return nil
	}

	out := make([]*Record, 0, n)
	idx := s.next
	if !s.full && idx == 0 {
		return nil
	}
	if s.full {
		idx = s.next
	} else {
		idx = s.next - 1
	}

	for i := 0; i < n; i++ {
		if idx < 0 {
			idx = s.cap - 1
		}
		if s.items[idx] == nil {
			break
		}
		if s.items[idx] != nil {
			out = append(out, s.items[idx])
		}
		idx--
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
