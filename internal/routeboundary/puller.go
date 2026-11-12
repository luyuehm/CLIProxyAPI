package routeboundary

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
)

// boundaryEndpoint is the KEEPER control-plane endpoint (RIC-2532-A) that
// publishes the per-API-key forced route-boundary snapshot for every CPA
// data-plane node. It is mounted under the /api/v1 group with
// adminOrManagementKeyMiddleware, so the CPA authenticates with the shared
// management key.
const boundaryEndpoint = "/api/v1/admin/api/route-boundaries"

// boundaryRow mirrors one JSON item in the KEEPER boundary delivery response
// (the shared contract fixed in RIC-2532). Unknown fields are ignored so
// KEEPER can add boundary dimensions later.
type boundaryRow struct {
	APIKey           string   `json:"apiKey"`
	DisplayKey       string   `json:"displayKey"`
	KeyAlias         string   `json:"keyAlias"`
	WorkspaceID      string   `json:"workspaceId"`
	Enabled          bool     `json:"enabled"`
	Revoked          bool     `json:"revoked"`
	AllowedProviders []string `json:"allowedProviders"`
	Strict           bool     `json:"strict"`
	OverrideAllowed  bool     `json:"overrideAllowed"`
}

// boundaryResponse mirrors the delivery response envelope.
type boundaryResponse struct {
	Items []boundaryRow `json:"items"`
	Total int           `json:"total"`
}

// PullerOptions configures the KEEPER route-boundary puller.
type PullerOptions struct {
	// KeeperURL is the KEEPER control-plane base URL (e.g.
	// http://127.0.0.1:4320 or http://keeper:8080 when behind APP_BASE_PATH).
	KeeperURL string
	// ManagementKey is the shared X-CPA-Management-Key used to authenticate
	// the CPA-to-KEEPER machine-to-machine pull.
	ManagementKey string
	// Interval is the poll period. The default is 5 seconds, giving
	// second-level boundary propagation to all CPA replicas.
	Interval time.Duration
	// HTTPClient is the client used for the pull; defaults to an http.Client
	// with a bounded timeout.
	HTTPClient *http.Client
}

const (
	// DefaultInterval is how often the puller re-fetches KEEPER boundaries.
	DefaultInterval = 5 * time.Second
	// defaultPullTimeout bounds a single boundary pull request.
	defaultPullTimeout = 5 * time.Second
)

func (o *PullerOptions) withDefaults() {
	if o.Interval <= 0 {
		o.Interval = DefaultInterval
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: defaultPullTimeout}
	}
}

// Puller polls the KEEPER boundary delivery endpoint and hot-reloads a Store.
// A failed pull latches the last good snapshot (fail-open): KEEPER being
// briefly down must not flap the gateway's routing behaviour, and a
// never-verified store stays empty so a KEEPER that starts after the gateway
// never degrades traffic at boot.
type Puller struct {
	opts   PullerOptions
	store  *Store
	cancel context.CancelFunc
	once   sync.Once
	wg     sync.WaitGroup
	pulls  atomic.Uint64
	fail   atomic.Uint64
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

// SyncOnce performs a single synchronous pull of KEEPER boundaries and
// atomically swaps the store snapshot. It is safe for concurrent use.
func (p *Puller) SyncOnce(ctx context.Context) error {
	if p == nil {
		return nil
	}
	p.pulls.Add(1)
	rows, err := p.fetch(ctx)
	if err != nil {
		p.fail.Add(1)
		logger.WithError(err).Debug("routeboundary: KEEPER boundary endpoint unreachable (keeping last snapshot)")
		return err
	}
	boundaries := make([]Boundary, 0, len(rows))
	for _, row := range rows {
		if row.APIKey == "" {
			continue
		}
		boundaries = append(boundaries, Boundary{
			APIKey:           row.APIKey,
			DisplayKey:       row.DisplayKey,
			KeyAlias:         row.KeyAlias,
			WorkspaceID:      row.WorkspaceID,
			Enabled:          row.Enabled,
			Revoked:          row.Revoked,
			AllowedProviders: row.AllowedProviders,
			Strict:           row.Strict,
			OverrideAllowed:  row.OverrideAllowed,
		})
	}
	p.store.Apply(boundaries)
	logger.WithField("keys", len(boundaries)).Debug("routeboundary: KEEPER boundary snapshot synced")
	return nil
}

// Stats returns pull counters for observability.
func (p *Puller) Stats() (pulls, failures uint64) {
	if p == nil {
		return 0, 0
	}
	return p.pulls.Load(), p.fail.Load()
}

// fetch GETs KEEPER's boundary delivery endpoint with the shared management
// key. A non-2xx response is a definitive failure; a transport error is
// transient. Both latch the last snapshot.
func (p *Puller) fetch(ctx context.Context) ([]boundaryRow, error) {
	url := strings.TrimRight(p.opts.KeeperURL, "/") + boundaryEndpoint
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("routeboundary: build boundary request: %w", err)
	}
	req.Header.Set("X-CPA-Management-Key", p.opts.ManagementKey)

	client := p.opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultPullTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("routeboundary: get boundary snapshot: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("routeboundary: boundary endpoint status=%d body=%s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var payload boundaryResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("routeboundary: decode boundary snapshot: %w", err)
	}
	return payload.Items, nil
}
