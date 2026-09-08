// Package dataratelimit implements an in-memory, per-API-key rate limiter and
// a KEEPER-backed QuotaExhausted circuit breaker for the CPA data plane.
//
// RIC-557: the data plane (Gin gateway) enforces, for every request carrying an
// Authorization: Bearer key, a per-key RPM (requests per minute) and a max
// in-flight concurrency. Over-limit requests get an immediate standard HTTP 429
// Too Many Requests with a Retry-After header. A separate blacklist consumes
// the KEEPER control plane's quota-exhausted status (sibling RIC-558's
// GET /api/v1/quota/status) every few seconds and breaks the circuit at
// second-level granularity for keys KEEPER reports as exceeded.
//
// All state is in-memory and bounded; nothing here writes to disk or to the
// upstream. The package is entirely opt-in: it is wired through an env-gated
// api.ServerOption (see setup.go), so when disabled the gateway behaves
// exactly as upstream.
package dataratelimit

import (
	"sync"
	"time"
)

// KeyLimits is the per-key rate limit configuration. Zero or negative values
// mean "unlimited" for that dimension.
type KeyLimits struct {
	// RPM is the maximum requests per minute for the key. <=0 disables the
	// RPM limit.
	RPM int64
	// MaxConcurrency is the maximum number of in-flight requests for the key.
	// <=0 disables the concurrency limit.
	MaxConcurrency int64
}

// LimiterOptions configures the in-memory limiter.
type LimiterOptions struct {
	// DefaultRPM applies to keys without an explicit per-key limit (i.e. keys
	// KEEPER has not registered). Zero disables the RPM limit.
	DefaultRPM int64
	// DefaultConcurrency applies to keys without an explicit per-key limit.
	// Zero disables the concurrency limit.
	DefaultConcurrency int64
	// MaxEntries bounds the number of tracked keys. The limiter evicts the
	// oldest idle buckets past this cap so memory stays bounded.
	MaxEntries int
	// IdleTTL is how long an idle bucket is retained before eviction.
	IdleTTL time.Duration
	// Now injects the clock for deterministic tests. Defaults to time.Now.
	Now func() time.Time
}

const (
	defaultLimiterMaxEntries = 10000
	defaultLimiterIdleTTL    = 30 * time.Minute
)

func (o *LimiterOptions) withDefaults() {
	if o.MaxEntries <= 0 {
		o.MaxEntries = defaultLimiterMaxEntries
	}
	if o.IdleTTL <= 0 {
		o.IdleTTL = defaultLimiterIdleTTL
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

type limiterBucket struct {
	// full marks a freshly-created bucket that has not had its capacity set
	// yet. The first refill fills it to capacity (burst semantics).
	full       bool
	tokens     float64
	lastRefill time.Time
	inFlight   int64
	lastSeen   time.Time
}

// Limiter is an in-memory per-API-key token-bucket limiter with concurrency
// accounting. Bucket identity is the raw API key value extracted from the
// Authorization header. All methods are safe for concurrent use.
type Limiter struct {
	mu      sync.Mutex
	now     func() time.Time
	buckets map[string]*limiterBucket

	defaultRPM         int64
	defaultConcurrency int64
	maxEntries         int
	idleTTL            time.Duration
}

// NewLimiter builds a Limiter with the given defaults.
func NewLimiter(opts LimiterOptions) *Limiter {
	opts.withDefaults()
	return &Limiter{
		now:                opts.Now,
		buckets:            make(map[string]*limiterBucket),
		defaultRPM:         opts.DefaultRPM,
		defaultConcurrency: opts.DefaultConcurrency,
		maxEntries:         opts.MaxEntries,
		idleTTL:            opts.IdleTTL,
	}
}

// Allow tries to acquire a request slot for the key using the limiter's
// default limits. It is a convenience wrapper over AllowWithLimits; callers
// that have per-key limits (e.g. from the KEEPER status puller) should pass
// them explicitly. On success the caller MUST call Release exactly once when
// the request finishes (defer in the middleware) to free the concurrency slot.
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	return l.AllowWithLimits(key, KeyLimits{})
}

// AllowWithLimits tries to acquire a request slot for the key under the given
// limits. A KeyLimits field of <=0 means "unlimited" for that dimension. When
// both fields are zero the limiter defaults apply. It returns false plus a
// Retry-After duration when either the concurrency ceiling or the token-bucket
// (RPM) is exhausted.
func (l *Limiter) AllowWithLimits(key string, limits KeyLimits) (bool, time.Duration) {
	if l == nil || key == "" {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b := l.bucketLocked(key, now)
	b.lastSeen = now

	rpm := limits.RPM
	maxConcurrency := limits.MaxConcurrency
	if rpm <= 0 {
		rpm = l.defaultRPM
	}
	if maxConcurrency <= 0 {
		maxConcurrency = l.defaultConcurrency
	}

	if maxConcurrency > 0 && b.inFlight >= maxConcurrency {
		return false, time.Second
	}

	if rpm > 0 {
		b.refill(now, rpm)
		if b.tokens < 1 {
			// Estimate the wait until the next token is available.
			perSecond := float64(rpm) / 60.0
			wait := time.Duration((1.0 - b.tokens) / perSecond * float64(time.Second))
			if wait < time.Second {
				wait = time.Second
			}
			return false, wait
		}
		b.tokens--
	}

	b.inFlight++
	return true, 0
}

// Release frees one in-flight slot previously acquired by Allow. It is safe
// to call with an arbitrary key; a no-op when the bucket is absent or idle.
func (l *Limiter) Release(key string) {
	if l == nil || key == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if b, ok := l.buckets[key]; ok && b.inFlight > 0 {
		b.inFlight--
	}
}

// InFlight reports the current in-flight count for a key (diagnostics).
func (l *Limiter) InFlight(key string) int64 {
	if l == nil || key == "" {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if b, ok := l.buckets[key]; ok {
		return b.inFlight
	}
	return 0
}

func (l *Limiter) bucketLocked(key string, now time.Time) *limiterBucket {
	b, ok := l.buckets[key]
	if ok {
		return b
	}
	if len(l.buckets) >= l.maxEntries {
		l.evictIdleLocked(now)
	}
	b = &limiterBucket{full: true, lastRefill: now}
	l.buckets[key] = b
	return b
}

// evictIdleLocked drops buckets with zero in-flight work whose lastSeen is
// older than idleTTL, oldest first. It is called only when the map is at its
// hard cap so the map never grows without bound under key rotation.
func (l *Limiter) evictIdleLocked(now time.Time) {
	var oldestKey string
	var oldestSeen time.Time
	for key, b := range l.buckets {
		if b.inFlight > 0 {
			continue
		}
		if now.Sub(b.lastSeen) < l.idleTTL {
			continue
		}
		if oldestKey == "" || b.lastSeen.Before(oldestSeen) {
			oldestKey = key
			oldestSeen = b.lastSeen
		}
	}
	if oldestKey != "" {
		delete(l.buckets, oldestKey)
	}
}

// refill adds tokens proportional to the elapsed time since the last refill,
// capped at the bucket capacity (one minute's worth of RPM). A fresh bucket
// starts full (burst semantics) so the first requests under the limit are not
// unfairly rejected.
func (b *limiterBucket) refill(now time.Time, rpm int64) {
	if rpm <= 0 {
		return
	}
	if b.full {
		b.full = false
		b.lastRefill = now
		b.tokens = float64(rpm)
		return
	}
	if b.lastRefill.IsZero() {
		b.lastRefill = now
		b.tokens = float64(rpm)
		return
	}
	elapsed := now.Sub(b.lastRefill)
	if elapsed <= 0 {
		return
	}
	b.lastRefill = now
	ratePerSecond := float64(rpm) / 60.0
	b.tokens += elapsed.Seconds() * ratePerSecond
	if b.tokens > float64(rpm) {
		b.tokens = float64(rpm)
	}
}
