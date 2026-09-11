package policies

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

// policyEndpoint is the KEEPER control-plane endpoint (RIC-596) that publishes
// the per-API-key rate-limit policy snapshot for every CPA data-plane node.
// It is mounted under the /api/v1 group with adminOrManagementKeyMiddleware,
// so the CPA authenticates with the shared management key.
const policyEndpoint = "/api/v1/admin/api/policies"

// policyRow mirrors one JSON item in the KEEPER policy delivery response.
// Unknown fields are ignored so KEEPER can add policy dimensions later.
type policyRow struct {
	APIKey          string  `json:"apiKey"`
	DisplayKey      string  `json:"displayKey"`
	KeyAlias        string  `json:"keyAlias"`
	Enabled         bool    `json:"enabled"`
	Revoked         bool    `json:"revoked"`
	RPMLimit        int64   `json:"rpmLimit"`
	MaxConcurrent   int64   `json:"maxConcurrent"`
	DailyTokenLimit int64   `json:"dailyTokenLimit"`
	MonthlyBudget   float64 `json:"monthlyBudget"`
	Configured      bool    `json:"configured"`
}

// policyResponse mirrors the response envelope.
type policyResponse struct {
	Items []policyRow `json:"items"`
	Total int         `json:"total"`
}

// PullerOptions configures the KEEPER policy puller.
type PullerOptions struct {
	// KeeperURL is the KEEPER control-plane base URL (e.g.
	// http://127.0.0.1:4320 or http://keeper:8080 when behind APP_BASE_PATH).
	KeeperURL string
	// ManagementKey is the shared X-CPA-Management-Key used to authenticate
	// the CPA-to-KEEPER machine-to-machine pull.
	ManagementKey string
	// Interval is the poll period. The default is 5 seconds, giving
	// second-level policy propagation to all CPA replicas.
	Interval time.Duration
	// HTTPClient is the client used for the pull; defaults to an http.Client
	// with a bounded timeout.
	HTTPClient *http.Client
	// Now injects the clock for deterministic tests.
	Now func() time.Time
}

const (
	// DefaultInterval is how often the puller re-fetches KEEPER policies.
	DefaultInterval = 5 * time.Second
	// defaultPullTimeout bounds a single policy pull request.
	defaultPullTimeout = 5 * time.Second
)

func (o *PullerOptions) withDefaults() {
	if o.Interval <= 0 {
		o.Interval = DefaultInterval
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: defaultPullTimeout}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

// Puller polls the KEEPER policy delivery endpoint and hot-reloads a Store.
// A failed pull latches the last good snapshot (fail-open): KEEPER being
// briefly down must not flap the gateway's admission behaviour, and a
// never-verified store stays empty so a KEEPER that starts after the gateway
// never degrades traffic at boot.
type Puller struct {
	opts    PullerOptions
	store   *Store
	cancel  context.CancelFunc
	once    sync.Once
	wg      sync.WaitGroup
	pulls   atomic.Uint64
	fail    atomic.Uint64
}

// NewPuller builds a puller against a KEEPER control plane and the store it
// will feed. A nil store disables the puller.
func NewPuller(store *Store, opts PullerOptions) *Puller {
	if store == nil {
		return nil
	}
	opts.withDefaults()
	return &Puller{opts: opts, store: store}
}

// Start performs a synchronous initial pull and begins the background poll
// loop. It is idempotent.
func (p *Puller) Start() {
	if p == nil {
		return
	}
	ctx := context.Background()
	p.once.Do(func() {
		childCtx, cancel := context.WithCancel(ctx)
		p.cancel = cancel
		// Best-effort initial sync; a failure keeps the store empty.
		_ = p.SyncOnce(childCtx)
		p.wg.Add(1)
		go p.loop(childCtx)
	})
}

// Stop ends the poll loop and waits for it to exit.
func (p *Puller) Stop() {
	if p == nil || p.cancel == nil {
		return
	}
	p.cancel()
	p.wg.Wait()
}

// loop polls KEEPER on the configured interval.
func (p *Puller) loop(ctx context.Context) {
	defer p.wg.Done()
	t := time.NewTicker(p.opts.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = p.SyncOnce(ctx)
		}
	}
}

// SyncOnce performs a single synchronous pull of KEEPER policies and atomically
// swaps the store snapshot. It is safe for concurrent use.
func (p *Puller) SyncOnce(ctx context.Context) error {
	if p == nil {
		return nil
	}
	p.pulls.Add(1)
	rows, err := p.fetch(ctx)
	if err != nil {
		p.fail.Add(1)
		logrus.WithError(err).Debug("policies: KEEPER policy endpoint unreachable (keeping last snapshot)")
		return err
	}
	policies := make([]Policy, 0, len(rows))
	for _, row := range rows {
		if row.APIKey == "" {
			continue
		}
		policies = append(policies, Policy{
			APIKey:          row.APIKey,
			Enabled:         row.Enabled && !row.Revoked,
			Revoked:         row.Revoked,
			RPMLimit:        row.RPMLimit,
			MaxConcurrent:   row.MaxConcurrent,
			DailyTokenLimit: row.DailyTokenLimit,
			MonthlyBudget:   row.MonthlyBudget,
		})
	}
	p.store.Apply(policies)
	logrus.WithField("keys", len(policies)).Debug("policies: KEEPER policy snapshot synced")
	return nil
}

// Stats returns pull counters for observability.
func (p *Puller) Stats() (pulls, failures uint64) {
	if p == nil {
		return 0, 0
	}
	return p.pulls.Load(), p.fail.Load()
}

// fetch GETs KEEPER's policy delivery endpoint with the shared management key.
// A non-2xx response is a definitive failure; a transport error is transient.
// Both latch the last snapshot.
func (p *Puller) fetch(ctx context.Context) ([]policyRow, error) {
	url := strings.TrimRight(p.opts.KeeperURL, "/") + policyEndpoint
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("policies: build policy request: %w", err)
	}
	req.Header.Set("X-CPA-Management-Key", p.opts.ManagementKey)

	client := p.opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultPullTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("policies: get policy snapshot: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("policies: policy endpoint status=%d body=%s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var payload policyResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("policies: decode policy snapshot: %w", err)
	}
	return payload.Items, nil
}