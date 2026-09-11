// Package policies implements the KEEPER → CPA policy delivery channel
// (RIC-596). A puller polls the KEEPER control plane's
// GET /admin/api/policies endpoint on a short interval and hot-reloads an
// in-memory per-API-key rate-limit policy store — RPM, max concurrency, daily
// token limit and monthly budget — without restarting the gateway.
//
// The package is modelled on the established internal/contentfilter syncer:
// a background poller owns the rule source, the request path reads an
// immutable snapshot under a read lock, and a failed pull latches the last
// good snapshot (fail-open) so a transient KEEPER outage never flaps the
// gateway's admission behaviour.
package policies

import (
	"sync"
)

// Policy is one API key's delivered rate-limit policy. Zero means unlimited.
// The fields mirror the KEEPER cpa_api_keys policy columns (RIC-559).
type Policy struct {
	APIKey          string
	Enabled         bool
	Revoked         bool
	RPMLimit        int64
	MaxConcurrent   int64
	DailyTokenLimit int64
	MonthlyBudget   float64
}

// snapshot is the immutable, atomically-swapped view of all delivered policies.
type snapshot map[string]Policy

// Store holds the latest delivered policy snapshot. It is safe for concurrent
// use: the background puller replaces the whole snapshot under a write lock,
// and the request path only takes a brief read lock — never network I/O.
type Store struct {
	mu       sync.RWMutex
	snapshot snapshot

	// verified marks that at least one successful pull has landed. Before
	// that, policies are not returned so a KEEPER that starts after the
	// gateway never degrades traffic at boot.
	verified bool
}

// NewStore returns an empty policy store.
func NewStore() *Store {
	return &Store{snapshot: make(snapshot)}
}

// Apply atomically replaces the store's snapshot. It is safe for concurrent use.
func (s *Store) Apply(policies []Policy) {
	if s == nil {
		return
	}
	next := make(snapshot, len(policies))
	for _, p := range policies {
		if p.APIKey == "" {
			continue
		}
		next[p.APIKey] = p
	}
	s.mu.Lock()
	s.snapshot = next
	s.verified = true
	s.mu.Unlock()
}

// Get returns the delivered policy for an API key, if any.
func (s *Store) Get(apiKey string) (Policy, bool) {
	if s == nil || apiKey == "" {
		return Policy{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.verified {
		return Policy{}, false
	}
	p, ok := s.snapshot[apiKey]
	return p, ok
}

// All returns a copy of every delivered policy keyed by API key.
func (s *Store) All() snapshot {
	if s == nil {
		return snapshot{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(snapshot, len(s.snapshot))
	for k, v := range s.snapshot {
		out[k] = v
	}
	return out
}

// Count reports the number of delivered policies.
func (s *Store) Count() int {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.snapshot)
}

// Verified reports whether at least one successful pull has landed.
func (s *Store) Verified() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.verified
}