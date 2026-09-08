package dataratelimit

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

// (env helpers live in setup.go for the package; strconv is not needed here.)

// quotaStatusEndpoint is the KEEPER control-plane endpoint (sibling RIC-558)
// that publishes per-API-key quota config and exceeded state for all CPA data
// plane nodes to poll.
const quotaStatusEndpoint = "/api/v1/quota/status"

// keeperStatusRow mirrors the JSON shape of one item in RIC-558's
// GET /api/v1/quota/status response. Unknown fields are ignored.
type keeperStatusRow struct {
	APIKey         string `json:"api_key"`
	DisplayKey     string `json:"display_key"`
	Enabled        bool   `json:"enabled"`
	RPM            int64  `json:"rpm"`
	MaxConcurrency int64  `json:"max_concurrency"`
	TPDLimit       int64  `json:"tpd_limit"`
	MonthlyBudget  int64  `json:"monthly_budget"`
	Exceeded       bool   `json:"exceeded"`
	ExceededReason string `json:"exceeded_reason"`
}

// keeperStatusResponse mirrors the response envelope of the quota/status
// endpoint.
type keeperStatusResponse struct {
	Items []keeperStatusRow `json:"items"`
	Total int               `json:"total"`
}

// BlacklistOptions configures the Keeper quota-status puller and the
// in-memory circuit-breaker blacklist.
type BlacklistOptions struct {
	// KeeperURL is the KEEPER control-plane base URL.
	KeeperURL string
	// ManagementKey is the shared X-CPA-Management-Key used to authenticate
	// the CPA-to-KEEPER machine-to-machine pull.
	ManagementKey string
	// Interval is the poll period. The default is 5 seconds, giving
	// second-level circuit breaking.
	Interval time.Duration
	// HTTPClient is the client used for the pull; defaults to an http.Client
	// with a bounded timeout.
	HTTPClient *http.Client
	// Now injects the clock for deterministic tests.
	Now func() time.Time
}

const (
	// defaultQuotaStatusInterval is how often the blacklist re-pulls KEEPER
	// quota status. 5s -> second-level breaker without hammering KEEPER.
	defaultQuotaStatusInterval = 5 * time.Second
	// defaultQuotaStatusTimeout bounds a single status pull request.
	defaultQuotaStatusTimeout = 5 * time.Second
)

func (o *BlacklistOptions) withDefaults() {
	if o.Interval <= 0 {
		o.Interval = defaultQuotaStatusInterval
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: defaultQuotaStatusTimeout}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

// Blacklist is an in-memory per-key circuit breaker fed by the KEEPER control
// plane. A key is blocked while KEEPER reports it as quota-exceeded; the pull
// loop refreshes the verdict every Interval so the breaker trips and releases
// at second-level granularity.
//
// It is safe for concurrent use: the background poller writes a frozen
// snapshot under a write lock, and the request path only takes a brief read
// lock — a request is never blocked on network I/O.
type Blacklist struct {
	opts BlacklistOptions

	// snapshot is the latest frozen view of blocked keys and per-key limits.
	// It is replaced atomically on every successful KEEPER pull; a failed pull
	// latches the last snapshot (fail-open-ish: the circuit keeps its last
	// verdict rather than flapping on a transient KEEPER outage).
	snapshotMu sync.RWMutex
	snapshot   blacklistSnapshot

	// verified marks that at least one successful pull has landed. Before
	// that, IsBlocked always returns false so a KEEPER that starts after the
	// gateway never blocks traffic at boot.
	verified atomic.Bool

	// pulls / failures are observability counters.
	pulls    atomic.Uint64
	failures atomic.Uint64

	cancel context.CancelFunc
	once   sync.Once
	wg     sync.WaitGroup
}

type blacklistSnapshot struct {
	blocked map[string]struct{}
	limits  map[string]KeyLimits
}

// NewBlacklist builds a puller against a KEEPER control plane. Callers should
// pass a nil blacklist to the middleware to keep the circuit open (no breaker)
// when no KEEPER control plane is configured.
func NewBlacklist(opts BlacklistOptions) *Blacklist {
	opts.withDefaults()
	return &Blacklist{
		opts: opts,
		snapshot: blacklistSnapshot{
			blocked: make(map[string]struct{}),
			limits:  make(map[string]KeyLimits),
		},
	}
}

// Start performs a synchronous initial pull and begins the background poll
// loop. It is idempotent. The synchronous first pull means the very first
// request served does not see a never-synced (unknown) verdict.
func (b *Blacklist) Start() *Blacklist {
	if b == nil {
		return nil
	}
	b.once.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		b.cancel = cancel
		// Best-effort initial sync; failures keep the blacklist open.
		_ = b.SyncOnce(ctx)
		b.wg.Add(1)
		go b.loop(ctx)
	})
	return b
}

// Stop ends the poll loop and waits for it to exit.
func (b *Blacklist) Stop() {
	if b == nil || b.cancel == nil {
		return
	}
	b.cancel()
	b.wg.Wait()
}

// loop polls KEEPER on the configured interval.
func (b *Blacklist) loop(ctx context.Context) {
	defer b.wg.Done()
	t := time.NewTicker(b.opts.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = b.SyncOnce(ctx)
		}
	}
}

// IsBlocked reports whether the key is currently quota-exhausted (circuit
// open). A nil blacklist — or a blacklist that has never successfully synced —
// reports false (no breaker / unknown → pass through), so a KEEPER that starts
// after the gateway never blocks traffic at boot.
func (b *Blacklist) IsBlocked(key string) bool {
	if b == nil || key == "" || !b.verified.Load() {
		return false
	}
	b.snapshotMu.RLock()
	_, blocked := b.snapshot.blocked[key]
	b.snapshotMu.RUnlock()
	return blocked
}

// Limits returns the last-published per-key limits for a key.
func (b *Blacklist) Limits(key string) (KeyLimits, bool) {
	if b == nil || key == "" {
		return KeyLimits{}, false
	}
	b.snapshotMu.RLock()
	limits, ok := b.snapshot.limits[key]
	b.snapshotMu.RUnlock()
	return limits, ok
}

// Stats returns pull counters for observability.
func (b *Blacklist) Stats() (pulls, failures uint64) {
	if b == nil {
		return 0, 0
	}
	return b.pulls.Load(), b.failures.Load()
}

// SyncOnce performs a single synchronous pull of KEEPER quota status and
// atomically swaps the blacklist and limits snapshot. It is safe for concurrent
// use.
func (b *Blacklist) SyncOnce(ctx context.Context) error {
	if b == nil {
		return nil
	}
	b.pulls.Add(1)
	items, err := b.fetchStatus(ctx)
	if err != nil {
		b.failures.Add(1)
		// A failed pull latches the last snapshot: KEEPER being briefly down
		// must not flap the circuit breaker, and a never-verified blacklist
		// stays open (IsBlocked=false) so a KEEPER that starts later does not
		// block the gateway at boot.
		logrus.WithError(err).Debug("dataratelimit: KEEPER quota status unreachable (keeping last verdict)")
		return err
	}
	b.apply(items)
	b.verified.Store(true)
	logrus.WithField("keys", len(items)).Debug("dataratelimit: KEEPER quota status synced")
	return nil
}

// apply freezes a new snapshot of blocked keys and limits.
func (b *Blacklist) apply(items []keeperStatusRow) {
	blocked := make(map[string]struct{}, len(items))
	limits := make(map[string]KeyLimits, len(items))
	for _, row := range items {
		if !row.Enabled || row.APIKey == "" {
			continue
		}
		limits[row.APIKey] = KeyLimits{
			RPM:            row.RPM,
			MaxConcurrency: row.MaxConcurrency,
		}
		if row.Exceeded {
			blocked[row.APIKey] = struct{}{}
		}
	}

	b.snapshotMu.Lock()
	b.snapshot.blocked = blocked
	b.snapshot.limits = limits
	b.snapshotMu.Unlock()
}

// fetchStatus GETs KEEPER's quota status endpoint with the shared management
// key. A non-2xx response (e.g. 401 when the key is wrong) is a definitive
// failure; a transport error is transient. Both latch the last snapshot.
func (b *Blacklist) fetchStatus(ctx context.Context) ([]keeperStatusRow, error) {
	url := strings.TrimRight(b.opts.KeeperURL, "/") + quotaStatusEndpoint
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("dataratelimit: build quota status request: %w", err)
	}
	req.Header.Set("X-CPA-Management-Key", b.opts.ManagementKey)

	client := b.opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultQuotaStatusTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dataratelimit: get quota status: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("dataratelimit: quota status status=%d body=%s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var payload keeperStatusResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("dataratelimit: decode quota status: %w", err)
	}
	return payload.Items, nil
}
