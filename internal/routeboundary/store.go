// Package routeboundary implements the KEEPER → CPA forced route-boundary
// channel (RIC-2532-A / OPENROUT-003). A puller polls the KEEPER control
// plane's GET /api/v1/admin/api/route-boundaries endpoint and hot-reloads an
// in-memory per-API-key provider allow-list. On the data plane the gate narrows
// every model-serving request's candidate provider pool to the delivered
// boundary: automatic fallback never leaves the pool, and a request whose
// boundary cannot be satisfied fails (403/503 route_boundary_violation)
// instead of degrading to an out-of-boundary provider.
//
// The package is modelled on the established internal/policies channel:
// a background poller owns the rule source, the request path reads an
// immutable snapshot under a read lock, and a failed pull latches the last
// good snapshot (fail-open) so a transient KEEPER outage never flaps the
// gateway's routing behaviour. The channel is off by default; an unconfigured
// build routes exactly as before and stamps no boundary headers.
package routeboundary

import (
	"strings"
	"sync"
)

// Boundary is one API key's delivered route boundary. A boundary constrains
// the key only when Enabled is true, Revoked is false, and
// AllowedProviders is non-empty; any other shape passes the request through
// untouched.
type Boundary struct {
	APIKey      string
	DisplayKey  string
	KeyAlias    string
	WorkspaceID string
	Enabled     bool
	Revoked     bool
	// AllowedProviders is the normalized (lower-cased, trimmed, de-duplicated)
	// set of provider keys the key may be routed to.
	AllowedProviders []string
	// Strict pins execution to the top-priority allowed provider: when that
	// provider is unavailable the request fails instead of trying the rest of
	// the list.
	Strict bool
	// OverrideAllowed admits the per-request X-OpenRouter-Route-Boundary
	// header, which grants a single audited pass-through to one named
	// provider, out of the boundary.
	OverrideAllowed bool
}

// Constrains reports whether this row enforces a hard boundary.
func (b Boundary) Constrains() bool {
	return b.Enabled && !b.Revoked && len(b.AllowedProviders) > 0
}

// Allows reports whether provider passes the boundary (case-insensitive).
func (b Boundary) Allows(provider string) bool {
	for _, allowed := range b.AllowedProviders {
		if allowed == normalizeProvider(provider) {
			return true
		}
	}
	return false
}

// normalizeProvider canonicalizes a provider key the way the auth manager
// normalizes them for selection: trimmed and lower-cased.
func normalizeProvider(provider string) string {
	return strings.ToLower(strings.TrimSpace(provider))
}

// normalizeProviders returns a deduplicated, normalized copy of providers,
// preserving first-seen order.
func normalizeProviders(providers []string) []string {
	if len(providers) == 0 {
		return nil
	}
	out := make([]string, 0, len(providers))
	seen := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		key := normalizeProvider(provider)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	return out
}

// snapshot is the immutable, atomically-swapped view of all delivered
// boundaries.
type snapshot map[string]Boundary

// Store holds the latest delivered boundary snapshot. It is safe for
// concurrent use: the background puller replaces the whole snapshot under a
// write lock, and the request path only takes a brief read lock — never
// network I/O.
type Store struct {
	mu       sync.RWMutex
	snapshot snapshot

	// verified marks that at least one successful pull has landed. Before
	// that, boundaries are not returned so a KEEPER that starts after the
	// gateway never degrades traffic at boot.
	verified bool
}

// NewStore returns an empty boundary store.
func NewStore() *Store {
	return &Store{snapshot: make(snapshot)}
}

// Apply atomically replaces the store's snapshot. It is safe for concurrent
// use.
func (s *Store) Apply(boundaries []Boundary) {
	if s == nil {
		return
	}
	next := make(snapshot, len(boundaries))
	for _, b := range boundaries {
		if b.APIKey == "" {
			continue
		}
		b.AllowedProviders = normalizeProviders(b.AllowedProviders)
		next[b.APIKey] = b
	}
	s.mu.Lock()
	s.snapshot = next
	s.verified = true
	s.mu.Unlock()
}

// Get returns the delivered boundary for an API key, if any. Before the first
// successful pull (Verified == false) it always reports not-found so an
// unverified store never constrains traffic.
func (s *Store) Get(apiKey string) (Boundary, bool) {
	if s == nil || apiKey == "" {
		return Boundary{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.verified {
		return Boundary{}, false
	}
	b, ok := s.snapshot[apiKey]
	return b, ok
}

// All returns a copy of every delivered boundary keyed by API key.
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

// Count reports the number of delivered boundaries.
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
