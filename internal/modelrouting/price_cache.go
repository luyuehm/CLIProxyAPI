package modelrouting

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// Price is one model's price row. Its JSON field names are frozen to match
// KEEPER's pricingEntryResponse exactly (keeper/internal/api/pricing.go): a
// tag drift would silently decode every price to 0, so the shape is pinned by
// TestPriceCacheKepperWireShape rather than eyeballed. All per-1M figures are
// USD and KEEPER's ModelPriceSetting is the single source of truth — no second
// price source is introduced.
type Price struct {
	// Model is the model identifier this row prices.
	Model string `json:"model"`
	// PromptPricePer1M is the input price per million tokens.
	PromptPricePer1M float64 `json:"prompt_price_per_1m"`
	// CompletionPricePer1M is the output price per million tokens.
	CompletionPricePer1M float64 `json:"completion_price_per_1m"`
	// CacheReadPricePer1M is the cache-read price per million tokens.
	CacheReadPricePer1M float64 `json:"cache_read_price_per_1m"`
	// CacheWritePricePer1M is the cache-write price per million tokens.
	CacheWritePricePer1M float64 `json:"cache_write_price_per_1m"`
	// Multiplier scales the base prices; KEEPER reports 1 when unset.
	Multiplier float64 `json:"price_multiplier"`
}

// Source is the single price source the cache syncs from. The production
// implementation is the KEEPER control-plane GET /api/v1/pricing endpoint
// (same management-key channel as internal/arbitrage and internal/policies);
// tests inject a fake so the cache is verified without a network.
type Source interface {
	// Fetch returns the current price table from the source. It must be a
	// complete, consistent snapshot: a partial result would tear the cache.
	Fetch(ctx context.Context) ([]Price, error)
}

// PriceSource is the KEEPER-backed Source: it GETs /api/v1/pricing on the
// KEEPER control plane with the shared management key.
type PriceSource struct {
	// KeeperURL is the KEEPER control-plane base URL (e.g.
	// http://keeper:8080/keeper when the deployment sets APP_BASE_PATH=/keeper).
	KeeperURL string
	// ManagementKey is the shared KEEPER management key used to authenticate
	// the pull.
	ManagementKey string
	// HTTPClient is the client used for the pull; defaults to a bounded
	// no-redirect client.
	HTTPClient *http.Client
}

// Fetch implements Source against KEEPER's pricing endpoint.
func (s *PriceSource) Fetch(ctx context.Context) ([]Price, error) {
	if s == nil || strings.TrimSpace(s.KeeperURL) == "" {
		return nil, fmt.Errorf("modelrouting: price source has no KEEPER URL")
	}
	url := strings.TrimRight(strings.TrimSpace(s.KeeperURL), "/") + pricingEndpoint
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("modelrouting: build price request: %w", err)
	}
	if s.ManagementKey != "" {
		req.Header.Set(managementKeyHeader, s.ManagementKey)
	}
	req.Header.Set("Accept", "application/json")

	client := s.HTTPClient
	if client == nil {
		client = &http.Client{
			Timeout: defaultPullTimeout,
			// KEEPER is a fixed origin by contract; following a redirect would
			// hand the management key to whatever host the redirect names.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("modelrouting: get pricing snapshot: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		slurped, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		// Report status and body length only; echoing the body verbatim would
		// let a hostile endpoint inject newlines/ANSI into our logs.
		return nil, fmt.Errorf("modelrouting: pricing endpoint status=%d body_len=%d",
			resp.StatusCode, len(slurped))
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, maxPricePayload))
	var envelope pricingEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return nil, fmt.Errorf("modelrouting: decode pricing snapshot: %w", err)
	}
	out := make([]Price, 0, len(envelope.Pricing))
	for _, row := range envelope.Pricing {
		if len(out) >= maxPriceItems {
			// Keep the snapshot useful rather than rejecting it outright; a
			// catalogue this large is a misconfiguration or a hostile payload,
			// and every row costs an Apply map write under the lock.
			logrus.WithField("cap", maxPriceItems).Warn("modelrouting: pricing snapshot truncated at the item cap")
			break
		}
		model := strings.TrimSpace(row.Model)
		if model == "" {
			// A row without a model name is unusable; skipping keeps the
			// snapshot free of map entries that always fall through.
			continue
		}
		row.Model = model
		out = append(out, row)
	}
	return out, nil
}

// pricingEnvelope mirrors KEEPER's GET /api/v1/pricing response body
// (keeper/internal/api/pricing.go pricingListResponse). Price's JSON tags are
// the KEEPER wire shape itself, so the row decodes straight into Price.
type pricingEnvelope struct {
	Pricing []Price `json:"pricing"`
}

const (
	// pricingEndpoint is KEEPER's pricing control-plane path, mounted under
	// the /api/v1 admin-protected group.
	pricingEndpoint = "/api/v1/pricing"
	// managementKeyHeader is the shared KEEPER management key header, the same
	// name internal/arbitrage and internal/policies use for their pulls.
	managementKeyHeader = "X-CPA-Management-Key"
	// maxPricePayload bounds the decoded body so a broken source cannot drive
	// unbounded memory use.
	maxPricePayload = 8 << 20
	// maxPriceItems bounds the decoded row count, for the same reason: the
	// byte cap alone still allows tens of thousands of minimal rows, each of
	// which forces a cache map write. Capping the count keeps one response
	// from stalling the request path.
	maxPriceItems = 5000
	// defaultPullTimeout bounds a single pricing pull.
	defaultPullTimeout = 10 * time.Second
)

// Snapshot is the immutable view a reader of the cache observes: one
// complete price table plus the instant it was last synced.
type Snapshot struct {
	// Prices maps model name to its Price row for this snapshot.
	Prices map[string]Price
	// SyncedAt is when this snapshot was taken; zero means never synced.
	SyncedAt time.Time
}

// Stale reports whether, as of now, this snapshot is older than the
// staleness horizon.
func (s Snapshot) Stale(now time.Time, horizon time.Duration) bool {
	if s.SyncedAt.IsZero() || horizon <= 0 {
		return true
	}
	return now.Sub(s.SyncedAt) > horizon
}

// PriceCache is a concurrency-safe, daily-synced model price cache. The
// whole table is swapped as one immutable generation under a write lock;
// readers take a brief read lock and receive a complete snapshot, so two
// goroutines reading concurrently can never see a torn table.
type PriceCache struct {
	mu       sync.RWMutex
	snapshot Snapshot

	// warnMu guards the rate-limited stale-warning state separately from
	// the snapshot lock, so the staleness check and its log can never
	// deadlock with Apply.
	warnMu        sync.Mutex
	lastStaleWarn time.Time
}

// NewPriceCache returns an empty cache (never-synced).
func NewPriceCache() *PriceCache {
	return &PriceCache{}
}

// Apply swaps the cache to a new complete snapshot. It is safe for
// concurrent use.
func (c *PriceCache) Apply(prices []Price, syncedAt time.Time) {
	if c == nil {
		return
	}
	table := make(map[string]Price, len(prices))
	for _, p := range prices {
		model := strings.TrimSpace(p.Model)
		if model == "" {
			continue
		}
		p.Model = model
		table[model] = p
	}
	c.mu.Lock()
	c.snapshot = Snapshot{Prices: table, SyncedAt: syncedAt}
	c.mu.Unlock()
}

// Get returns the full snapshot observed by this call: a complete,
// immutable table plus its sync time. Prices is nil (never synced) when no
// snapshot is in force.
func (c *PriceCache) Get() (map[string]Price, time.Time) {
	if c == nil {
		return nil, time.Time{}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.snapshot.Prices, c.snapshot.SyncedAt
}

// IsStale reports whether the current snapshot is older than horizon as of
// now, and marks that fact with a rate-limited warning log. A never-synced
// cache is stale.
func (c *PriceCache) IsStale(now time.Time, horizon time.Duration) bool {
	if c == nil {
		return true
	}
	c.mu.RLock()
	snapshot := c.snapshot
	c.mu.RUnlock()
	stale := snapshot.Stale(now, horizon)
	if stale {
		c.warnStale(now, snapshot.SyncedAt, horizon)
	}
	return stale
}

// PriceInfo is the result of looking a model up in the price cache.
type PriceInfo struct {
	// Price is the model's row.
	Price Price
	// Found is true when the model is present in the current snapshot.
	Found bool
	// Stale is true when the snapshot is older than the horizon.
	Stale bool
}

// Price returns the price row for a model plus staleness information. A
// nil cache reports an unfound, stale result: the caller then falls back
// to default routing rather than acting on a missing price.
func (c *PriceCache) Price(model string, now time.Time, horizon time.Duration) PriceInfo {
	if c == nil {
		return PriceInfo{Stale: true}
	}
	c.mu.RLock()
	prices := c.snapshot.Prices
	p, found := prices[strings.TrimSpace(model)]
	c.mu.RUnlock()
	return PriceInfo{Price: p, Found: found, Stale: c.IsStale(now, horizon)}
}

// warnStale emits the staleness warning at most once per horizon period,
// so a long-stale window cannot spam the log on every read. It uses its
// own lock so the warning state never deadlocks with the snapshot lock.
func (c *PriceCache) warnStale(now, syncedAt time.Time, horizon time.Duration) {
	if horizon <= 0 {
		return
	}
	c.warnMu.Lock()
	defer c.warnMu.Unlock()
	if now.Sub(c.lastStaleWarn) < horizon {
		return
	}
	c.lastStaleWarn = now
	logrus.WithField("synced_at", syncedAt).
		Warn("modelrouting: price cache snapshot is stale; prices may be out of date")
}

// Syncer pulls a price table from a Source on a (daily) schedule and hot
// reloads a PriceCache. A failed sync keeps the last good snapshot, so a
// KEEPER outage must not clear routing's price basis.
type Syncer struct {
	cache    *PriceCache
	source   Source
	interval time.Duration
	horizon  time.Duration

	mu      sync.Mutex
	cancel  context.CancelFunc
	started bool
	wg      sync.WaitGroup

	now func() time.Time
}

// SyncerOptions configures the price syncer.
type SyncerOptions struct {
	// Interval is the sync period; DailyInterval when unset.
	Interval time.Duration
	// StalenessHorizon is how old a snapshot may be before readers report
	// it stale; DailyInterval when unset.
	StalenessHorizon time.Duration
	// Now injects the clock for deterministic tests.
	Now func() time.Time
}

// DailyInterval is the default price sync period.
const DailyInterval = 24 * time.Hour

func (o *SyncerOptions) withDefaults() {
	if o.Interval <= 0 {
		o.Interval = DailyInterval
	}
	if o.StalenessHorizon <= 0 {
		o.StalenessHorizon = DailyInterval
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

// NewSyncer builds a syncer feeding cache from source. A nil cache or a nil
// source disables the syncer.
func NewSyncer(cache *PriceCache, source Source, opts SyncerOptions) *Syncer {
	if cache == nil || source == nil {
		return nil
	}
	opts.withDefaults()
	return &Syncer{
		cache:    cache,
		source:   source,
		interval: opts.Interval,
		horizon:  opts.StalenessHorizon,
		now:      opts.Now,
	}
}

// SetNow injects the clock. Intended for tests only; it takes the lifecycle
// lock so a swap cannot race a running loop.
func (s *Syncer) SetNow(now func() time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if now != nil {
		s.now = now
	}
}

// nowFunc returns the injected clock under the lifecycle lock, so the pull
// loop never reads a field a concurrent SetNow is writing.
func (s *Syncer) nowFunc() func() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.now
}

// Start performs one synchronous initial sync and begins the background
// loop. It is idempotent.
func (s *Syncer) Start() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.started = true
	s.mu.Unlock()

	// Best-effort initial sync; a failure keeps the cache empty so routing
	// falls back to the default strategy rather than acting on a torn table.
	_ = s.SyncOnce(ctx)
	s.wg.Add(1)
	go s.loop(ctx)
}

// Stop ends the sync loop and waits for it to exit.
func (s *Syncer) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	s.wg.Wait()
}

func (s *Syncer) loop(ctx context.Context) {
	defer s.wg.Done()
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = s.SyncOnce(ctx)
		}
	}
}

// SyncOnce performs one pull and atomically swaps the cache snapshot. It is
// safe for concurrent use.
func (s *Syncer) SyncOnce(ctx context.Context) error {
	if s == nil {
		return nil
	}
	prices, err := s.source.Fetch(ctx)
	if err != nil {
		logrus.WithError(err).Debug("modelrouting: KEEPER pricing unreachable (keeping last snapshot)")
		return err
	}
	s.cache.Apply(prices, s.nowFunc()())
	logrus.WithField("models", len(prices)).Debug("modelrouting: price snapshot synced")
	return nil
}
