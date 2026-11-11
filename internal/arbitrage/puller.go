// Package arbitrage implements the CLIProxyAPI forwarding-side consumer of the
// KEEPER cross-cloud cost arbitrage engine (RIC-2458).
//
// KEEPER (enterprise-keeper) already computes, per model, the cheapest healthy
// Provider and records it as an audit event — but it never carries user traffic,
// so on its own that decision cannot lower a single bill. This package closes the
// loop: it polls KEEPER's machine-to-machine decision delivery endpoint on a short
// interval, caches the current "model → Provider/Endpoint/AuthRef" mapping in
// memory, and makes the live upstream target follow it.
//
// Design rules, in priority order:
//
//  1. Availability first. Every failure mode — KEEPER unreachable, snapshot
//     expired, malformed payload, model not routed — degrades to the gateway's
//     existing default routing. Missing decisions never fail a request.
//  2. Bounded request cost. The request path only takes a read lock over an
//     immutable snapshot and does map lookups; it never performs network I/O.
//  3. No plaintext secrets. ProviderPrice.AuthRef is a management-plane
//     credential *reference name*. It is resolved against the local credential
//     pool at request time; the delivery payload carries no key material.
//
// The package is off by default (no environment variable, no behaviour change),
// matching the established internal/policies and internal/dataratelimit channels.
package arbitrage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"
)

// deliveryEndpoint is the KEEPER machine-to-machine decision delivery path
// (RIC-2458). It is mounted under the /api/v1 group behind
// adminOrManagementKeyMiddleware, so the gateway authenticates with the same
// shared management key it already uses for the policy and quota channels.
const deliveryEndpoint = "/api/v1/arbitrage/decisions/delivery"

// RequestHeaderName is the shared management key header KEEPER validates.
const RequestHeaderName = "X-CPA-Management-Key"

const (
	// DefaultInterval is how often the gateway re-pulls decisions. KEEPER's own
	// re-bid cycle defaults to 5m; pulling faster just shortens propagation delay.
	DefaultInterval = 30 * time.Second
	// defaultPullTimeout bounds a single decision pull.
	defaultPullTimeout = 5 * time.Second
	// DefaultStalenessFactor multiplies KEEPER's advertised interval to decide
	// when a locally cached snapshot is too old to trust. 3 tolerates two lost
	// cycles before falling back, so a brief KEEPER outage does not flap routing.
	DefaultStalenessFactor = 3
	// maxSnapshotSkew bounds how far in the future we accept a generated_at
	// stamp. A wildly future stamp would otherwise pin a snapshot as fresh
	// forever if the gateway clock is behind.
	maxSnapshotSkew = 5 * time.Minute
	// maxDecisionPayload bounds the decoded body so a hostile or broken KEEPER
	// cannot drive unbounded memory use.
	maxDecisionPayload = 8 << 20
	// maxDecisionItems bounds the item count. The byte cap alone still allows
	// ~100k minimal decisions, each of which forces a manager write-lock cycle;
	// capping the count keeps one response from stalling the request path.
	maxDecisionItems = 5000
	// minAcceptedInterval / maxAcceptedInterval clamp KEEPER's advertised
	// interval before it drives the freshness window. Without an upper bound a
	// huge interval_seconds overflows time.Duration and pins the snapshot as
	// fresh for centuries, defeating the fall-back-to-default guarantee.
	minAcceptedInterval = 5.0
	maxAcceptedInterval = 3600.0
	// maxFreshnessWindow caps the total freshness window regardless of the
	// advertised interval and factor, so stale pricing cannot be acted on
	// indefinitely.
	maxFreshnessWindow = time.Hour
)

// Decision is one model's current arbitration result, as delivered by KEEPER.
type Decision struct {
	Model      string  `json:"model"`
	Provider   string  `json:"provider"`
	Endpoint   string  `json:"endpoint"`
	AuthRef    string  `json:"auth_ref"`
	PricePer1M float64 `json:"price_per_1m"`
	Reason     string  `json:"reason"`
	Fallback   bool    `json:"fallback"`
}

// snapshotResponse mirrors KEEPER's DecisionDeliverySnapshot envelope. Unknown
// fields are ignored so KEEPER can extend the contract without breaking older
// gateway builds.
type snapshotResponse struct {
	Items           []Decision `json:"items"`
	Total           int        `json:"total"`
	GeneratedAt     time.Time  `json:"generated_at"`
	IntervalSeconds float64    `json:"interval_seconds"`
}

// DecisionView is the read-only shape returned to management/observability
// callers. It deliberately omits AuthRef: the credential reference name is for
// local resolution only and must not leak into an HTTP surface.
type DecisionView struct {
	Model      string  `json:"model"`
	Provider   string  `json:"provider"`
	Endpoint   string  `json:"endpoint"`
	PricePer1M float64 `json:"price_per_1m"`
	Reason     string  `json:"reason"`
	Fallback   bool    `json:"fallback"`
}

// Store holds the latest delivered decision snapshot.
//
// The background puller replaces the whole snapshot under a write lock; the
// request path takes a brief read lock and performs map lookups only. A snapshot
// is only trustworthy while it is both verified (at least one successful pull)
// and fresh (see freshAt) — an unverified or expired store reports no decision,
// which routes the request through the gateway's normal path.
type Store struct {
	mu       sync.RWMutex
	byModel  map[string]Decision
	verified bool
	// freshAt is the instant after which the snapshot must no longer be used.
	// Zero means "no usable snapshot".
	freshAt time.Time
	// serverGeneratedAt is KEEPER's own stamp, kept for observability so an
	// operator can tell "KEEPER is stale" from "our clock is stale".
	serverGeneratedAt time.Time
	// intervalSeconds is KEEPER's advertised re-bid interval.
	intervalSeconds float64
}

// NewStore returns an empty decision store.
func NewStore() *Store {
	return &Store{byModel: make(map[string]Decision)}
}

// Apply atomically replaces the snapshot. now and stalenessFactor come from the
// puller so tests can inject a clock and a short window.
func (s *Store) Apply(items []Decision, generatedAt time.Time, intervalSeconds float64, now time.Time, stalenessFactor int) {
	if s == nil {
		return
	}
	next := make(map[string]Decision, len(items))
	for _, item := range items {
		if len(next) >= maxDecisionItems {
			// Keep the snapshot useful rather than rejecting it outright; a
			// catalogue this large is a misconfiguration or a hostile payload.
			logrus.WithField("cap", maxDecisionItems).Warn("arbitrage: decision snapshot truncated at the item cap")
			break
		}
		model := strings.TrimSpace(item.Model)
		if model == "" {
			continue
		}
		item.Model = model
		item.Provider = strings.TrimSpace(item.Provider)
		item.Endpoint = strings.TrimSpace(item.Endpoint)
		item.AuthRef = strings.TrimSpace(item.AuthRef)
		// A decision without a target provider is not actionable; keeping it
		// would only add a map entry that always falls through.
		if item.Provider == "" {
			continue
		}
		next[model] = item
	}

	// Clamp the advertised interval: too small makes every pull its own window,
	// too large overflows the duration and would pin the snapshot as fresh
	// indefinitely.
	if intervalSeconds <= 0 || intervalSeconds < minAcceptedInterval || intervalSeconds > maxAcceptedInterval {
		intervalSeconds = DefaultInterval.Seconds()
	}
	if stalenessFactor <= 1 {
		stalenessFactor = DefaultStalenessFactor
	}
	// Staleness is measured from the newer of our receive time and KEEPER's
	// stamp, clamped so a future-dated stamp cannot extend freshness forever.
	stamp := now
	if generatedAt.After(stamp) && generatedAt.Before(now.Add(maxSnapshotSkew)) {
		stamp = generatedAt
	}
	window := time.Duration(intervalSeconds * float64(time.Second) * float64(stalenessFactor))
	if window <= 0 || window > maxFreshnessWindow {
		window = maxFreshnessWindow
	}

	s.mu.Lock()
	s.byModel = next
	s.verified = true
	s.freshAt = stamp.Add(window)
	s.serverGeneratedAt = generatedAt
	s.intervalSeconds = intervalSeconds
	s.mu.Unlock()
}

// Decision returns the delivered decision for a model when the snapshot is both
// verified and still fresh. ok=false means the caller must use default routing.
func (s *Store) Decision(model string, now time.Time) (Decision, bool) {
	if s == nil {
		return Decision{}, false
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return Decision{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.verified || s.freshAt.IsZero() || !now.Before(s.freshAt) {
		return Decision{}, false
	}
	decision, ok := s.byModel[model]
	return decision, ok
}

// Usable reports whether the snapshot is verified and still fresh, i.e. whether
// any decision may be acted on right now.
func (s *Store) Usable(now time.Time) bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.verified && !s.freshAt.IsZero() && now.Before(s.freshAt)
}

// All returns a copy of every delivered decision, including AuthRef, for local
// credential resolution. It must never be exposed over an HTTP surface.
func (s *Store) All() []Decision {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Decision, 0, len(s.byModel))
	for _, decision := range s.byModel {
		out = append(out, decision)
	}
	return out
}

// Views returns a copy of every delivered decision without credential refs, for
// management/observability endpoints.
func (s *Store) Views() []DecisionView {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]DecisionView, 0, len(s.byModel))
	for _, decision := range s.byModel {
		out = append(out, DecisionView{
			Model:      decision.Model,
			Provider:   decision.Provider,
			Endpoint:   decision.Endpoint,
			PricePer1M: decision.PricePer1M,
			Reason:     decision.Reason,
			Fallback:   decision.Fallback,
		})
	}
	return out
}

// Stats describes the store for observability and diagnostics.
type Stats struct {
	Verified          bool      `json:"verified"`
	Fresh             bool      `json:"fresh"`
	Models            int       `json:"models"`
	FreshAt           time.Time `json:"fresh_at"`
	ServerGeneratedAt time.Time `json:"server_generated_at"`
	IntervalSeconds   float64   `json:"interval_seconds"`
	Pulls             uint64    `json:"pulls"`
	Failures          uint64    `json:"failures"`
}

// Stats returns a point-in-time description of the store, including pull
// counters, and reports whether the snapshot is currently usable.
func (s *Store) Stats(now time.Time) Stats {
	if s == nil {
		return Stats{}
	}
	s.mu.RLock()
	stats := Stats{
		Verified:          s.verified,
		Models:            len(s.byModel),
		FreshAt:           s.freshAt,
		ServerGeneratedAt: s.serverGeneratedAt,
		IntervalSeconds:   s.intervalSeconds,
	}
	s.mu.RUnlock()
	stats.Fresh = stats.Verified && !stats.FreshAt.IsZero() && now.Before(stats.FreshAt)
	return stats
}

// PullerOptions configures the decision puller.
type PullerOptions struct {
	// KeeperURL is the KEEPER base URL (e.g. http://keeper:8080/keeper when the
	// deployment sets APP_BASE_PATH=/keeper).
	KeeperURL string
	// ManagementKey is the shared X-CPA-Management-Key used for the pull.
	ManagementKey string
	// Interval is the poll period; DefaultInterval when unset.
	Interval time.Duration
	// StalenessFactor multiplies KEEPER's advertised interval to expire the
	// local snapshot; DefaultStalenessFactor when unset.
	StalenessFactor int
	// HTTPClient is used for the pull; defaults to a bounded client.
	HTTPClient *http.Client
	// Now injects the clock for deterministic tests.
	Now func() time.Time
	// AfterSync runs after every successful snapshot swap. The service uses it to
	// reconcile runtime credentials with the new decisions; failures there must
	// not affect the pull itself.
	AfterSync func()
}

func (o *PullerOptions) withDefaults() {
	if o.Interval <= 0 {
		o.Interval = DefaultInterval
	}
	if o.StalenessFactor <= 1 {
		o.StalenessFactor = DefaultStalenessFactor
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{
			Timeout: defaultPullTimeout,
			// Never follow a redirect. Go preserves custom headers across a
			// redirect, so following one would hand the shared management key to
			// whatever host the redirect names. KEEPER is a fixed origin by
			// contract; a redirect means a misconfigured or hostile endpoint.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

// Puller polls KEEPER's decision delivery endpoint and hot-reloads a Store.
//
// A failed pull keeps the last good snapshot: KEEPER being briefly down must not
// flap routing mid-flight, and the snapshot's own freshness window then decides
// when to stop trusting it. A never-verified store stays empty, so a KEEPER that
// starts after the gateway never affects traffic at boot.
type Puller struct {
	opts  PullerOptions
	store *Store
	now   func() time.Time

	// mu guards the lifecycle fields below. Start and Stop may be called from
	// different goroutines (startup vs. shutdown hook), and sync.Once orders only
	// other Do calls, not a concurrent read of cancel.
	mu      sync.Mutex
	cancel  context.CancelFunc
	started bool
	wg      sync.WaitGroup

	pulls  atomic.Uint64
	failed atomic.Uint64
}

// NewPuller builds a puller against a KEEPER control plane. A nil store
// disables the puller.
func NewPuller(store *Store, opts PullerOptions) *Puller {
	if store == nil {
		return nil
	}
	opts.withDefaults()
	return &Puller{opts: opts, store: store, now: opts.Now}
}

// Start performs a synchronous initial pull and begins the background loop. It
// is idempotent.
func (p *Puller) Start() {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.started = true
	p.mu.Unlock()

	// Best-effort initial sync; a failure keeps the store empty so the gateway
	// keeps its default routing.
	_ = p.SyncOnce(ctx)
	p.wg.Add(1)
	go p.loop(ctx)
}

// Stop ends the poll loop and waits for it to exit.
func (p *Puller) Stop() {
	if p == nil {
		return
	}
	p.mu.Lock()
	cancel := p.cancel
	p.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	p.wg.Wait()
}

func (p *Puller) loop(ctx context.Context) {
	defer p.wg.Done()
	ticker := time.NewTicker(p.opts.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = p.SyncOnce(ctx)
		}
	}
}

// SyncOnce performs one synchronous pull and atomically swaps the store
// snapshot. It is safe for concurrent use.
func (p *Puller) SyncOnce(ctx context.Context) error {
	if p == nil {
		return nil
	}
	p.pulls.Add(1)
	snapshot, err := p.fetch(ctx)
	if err != nil {
		p.failed.Add(1)
		logrus.WithError(err).Debug("arbitrage: KEEPER decision endpoint unreachable (keeping last snapshot)")
		return err
	}
	p.store.Apply(snapshot.Items, snapshot.GeneratedAt, snapshot.IntervalSeconds, p.now(), p.opts.StalenessFactor)
	if p.opts.AfterSync != nil {
		p.opts.AfterSync()
	}
	logrus.WithField("models", len(snapshot.Items)).Debug("arbitrage: decision snapshot synced")
	return nil
}

// fetch GETs KEEPER's decision delivery endpoint with the shared management key.
func (p *Puller) fetch(ctx context.Context) (snapshotResponse, error) {
	url := strings.TrimRight(p.opts.KeeperURL, "/") + deliveryEndpoint
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return snapshotResponse{}, fmt.Errorf("arbitrage: build decision request: %w", err)
	}
	req.Header.Set(RequestHeaderName, p.opts.ManagementKey)
	req.Header.Set("Accept", "application/json")

	client := p.opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultPullTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return snapshotResponse{}, fmt.Errorf("arbitrage: get decision snapshot: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Report the status and body length only. Echoing the body verbatim would
		// let a broken or hostile endpoint inject newlines/ANSI into our logs.
		slurped, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return snapshotResponse{}, fmt.Errorf("arbitrage: decision endpoint status=%d body_len=%d",
			resp.StatusCode, len(slurped))
	}
	var snapshot snapshotResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxDecisionPayload)).Decode(&snapshot); err != nil {
		return snapshotResponse{}, fmt.Errorf("arbitrage: decode decision snapshot: %w", err)
	}
	return snapshot, nil
}

// Counters returns the pull and failure counts for observability.
func (p *Puller) Counters() (pulls, failures uint64) {
	if p == nil {
		return 0, 0
	}
	return p.pulls.Load(), p.failed.Load()
}
