package proxy

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// ErrTooManyStreams is returned when a key already holds the maximum number of
// concurrently retained streams and none of them can be evicted.
var ErrTooManyStreams = errors.New("too many concurrent streams for this API key")

const (
	// DefaultIdleTTL is how long a finished or client-less session is retained
	// before it is released. It is an idle window, not an absolute deadline: a
	// long generation that keeps producing frames never expires, which is what
	// keeps long tasks resumable (see docs/stream-resume.md).
	DefaultIdleTTL = 2 * time.Minute

	// DefaultSweepInterval is how often expired sessions are reclaimed.
	DefaultSweepInterval = 15 * time.Second

	// DefaultMaxSessionBytes bounds the retained frame window per session.
	DefaultMaxSessionBytes = 1 << 20 // 1 MiB

	// DefaultMaxSessions bounds the total number of retained sessions.
	DefaultMaxSessions = 1024

	// DefaultMaxSessionsPerKey bounds one principal's retained sessions.
	DefaultMaxSessionsPerKey = 16

	// streamIDBytes is the entropy of a stream identifier.
	streamIDBytes = 32
)

// RegistryOptions configures a Registry. Zero values select the defaults.
type RegistryOptions struct {
	IdleTTL           time.Duration
	SweepInterval     time.Duration
	MaxSessionBytes   int
	MaxSessions       int
	MaxSessionsPerKey int
	Logger            *log.Logger
	// Now overrides the clock (tests).
	Now func() time.Time
}

// Registry owns the retained stream sessions of a gateway instance.
type Registry struct {
	opts  RegistryOptions
	salt  []byte
	log   *log.Logger
	now   func() time.Time
	mu    sync.Mutex
	byID  map[string]*Session
	stop  chan struct{}
	done  chan struct{}
	once  sync.Once
	start sync.Once
}

// NewRegistry creates a registry and starts its reclamation loop.
func NewRegistry(opts RegistryOptions) *Registry {
	if opts.IdleTTL <= 0 {
		opts.IdleTTL = DefaultIdleTTL
	}
	if opts.SweepInterval <= 0 {
		opts.SweepInterval = DefaultSweepInterval
	}
	if opts.MaxSessionBytes <= 0 {
		opts.MaxSessionBytes = DefaultMaxSessionBytes
	}
	if opts.MaxSessions <= 0 {
		opts.MaxSessions = DefaultMaxSessions
	}
	if opts.MaxSessionsPerKey <= 0 {
		opts.MaxSessionsPerKey = DefaultMaxSessionsPerKey
	}
	logger := opts.Logger
	if logger == nil {
		logger = log.StandardLogger()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		// A random salt is a hardening measure, not a correctness requirement:
		// fall back to a fixed per-process value rather than failing startup.
		sum := sha256.Sum256([]byte("cli-proxy-api/stream-resume"))
		copy(salt, sum[:])
	}
	r := &Registry{
		opts: opts,
		salt: salt,
		log:  logger,
		now:  now,
		byID: make(map[string]*Session),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	go r.sweepLoop()
	return r
}

// Fingerprint derives a stable, non-reversible identifier for an API key. The
// plaintext key is never retained by the registry.
func (r *Registry) Fingerprint(key string) string {
	mac := hmac.New(sha256.New, r.salt)
	mac.Write([]byte(key))
	return hex.EncodeToString(mac.Sum(nil))
}

// Open registers a new session bound to keyID. cancel releases the detached
// upstream execution when the session expires or is closed.
func (r *Registry) Open(keyID string, cancel context.CancelFunc) (*Session, error) {
	id, err := newStreamID()
	if err != nil {
		return nil, err
	}
	now := r.now()
	session := &Session{
		id:         id,
		keyID:      keyID,
		maxBytes:   r.opts.MaxSessionBytes,
		createdAt:  now,
		lastActive: now,
		changed:    make(chan struct{}),
		cancel:     cancel,
		now:        r.now,
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if errEvict := r.makeRoomLocked(keyID); errEvict != nil {
		return nil, errEvict
	}
	session.onClose = r.remove
	r.byID[id] = session
	return session, nil
}

// Lookup returns the session with the given ID.
func (r *Registry) Lookup(id string) (*Session, bool) {
	if id == "" {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.byID[id]
	return session, ok
}

// Len returns the number of retained sessions.
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byID)
}

// Close releases every retained session and stops the reclamation loop.
func (r *Registry) Close() {
	r.once.Do(func() {
		close(r.stop)
		<-r.done
	})
	r.mu.Lock()
	sessions := make([]*Session, 0, len(r.byID))
	for _, session := range r.byID {
		sessions = append(sessions, session)
	}
	r.byID = make(map[string]*Session)
	r.mu.Unlock()
	for _, session := range sessions {
		session.Close()
	}
}

// remove drops a closed session from the index.
func (r *Registry) remove(session *Session) {
	r.mu.Lock()
	if current, ok := r.byID[session.id]; ok && current == session {
		delete(r.byID, session.id)
	}
	r.mu.Unlock()
}

// makeRoomLocked enforces the per-key and global session caps, evicting the
// oldest client-less session when possible. Callers hold r.mu.
func (r *Registry) makeRoomLocked(keyID string) error {
	if r.countKeyLocked(keyID) >= r.opts.MaxSessionsPerKey {
		if !r.evictLocked(func(s *Session) bool { return s.keyID == keyID }) {
			return ErrTooManyStreams
		}
	}
	for len(r.byID) >= r.opts.MaxSessions {
		if !r.evictLocked(func(*Session) bool { return true }) {
			return ErrTooManyStreams
		}
	}
	return nil
}

// evictLocked closes the oldest session matching match that has no attached
// reader. It reports whether a session was evicted. Callers hold r.mu.
func (r *Registry) evictLocked(match func(*Session) bool) bool {
	var oldest *Session
	for _, session := range r.byID {
		if !match(session) || session.Subscribers() > 0 || session.Held() || session.Closed() {
			continue
		}
		if oldest == nil || session.LastActive().Before(oldest.LastActive()) {
			oldest = session
		}
	}
	if oldest == nil {
		return false
	}
	delete(r.byID, oldest.id)
	r.mu.Unlock()
	oldest.Close()
	r.mu.Lock()
	return true
}

func (r *Registry) countKeyLocked(keyID string) int {
	count := 0
	for _, session := range r.byID {
		if session.keyID == keyID {
			count++
		}
	}
	return count
}

// sweepLoop reclaims sessions whose retention window lapsed.
func (r *Registry) sweepLoop() {
	defer close(r.done)
	ticker := time.NewTicker(r.opts.SweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-ticker.C:
			r.sweep()
		}
	}
}

func (r *Registry) sweep() {
	now := r.now()
	var expired []*Session
	r.mu.Lock()
	for id, session := range r.byID {
		if session.Subscribers() > 0 || session.Held() {
			continue
		}
		if session.IdleFor(now) >= r.opts.IdleTTL {
			delete(r.byID, id)
			expired = append(expired, session)
		}
	}
	r.mu.Unlock()
	for _, session := range expired {
		session.Close()
	}
	if len(expired) > 0 {
		r.log.WithField("expired_sessions", len(expired)).Debug("stream resume registry swept idle sessions")
	}
}

// newStreamID returns an unguessable URL-safe stream identifier.
func newStreamID() (string, error) {
	buf := make([]byte, streamIDBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
