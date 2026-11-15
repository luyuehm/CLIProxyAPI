package modelrouting

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSource is an injectable Source so the cache is verified without a
// network.
type fakeSource struct {
	prices []Price
	err    error
}

func (f *fakeSource) Fetch(context.Context) ([]Price, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([]Price, len(f.prices))
	copy(out, f.prices)
	return out, nil
}

// TestPriceCacheKeeperWireShape pins KEEPER's GET /api/v1/pricing JSON field
// names against the Price struct's tags. If either side of that contract
// drifts, the real sync silently decodes every price to 0 — so this test is
// the guard that fails on tag drift. The sample is KEEPER's exact
// pricingEntryResponse shape (keeper/internal/api/pricing.go).
func TestPriceCacheKeeperWireShape(t *testing.T) {
	body := []byte(`{"pricing":[{"model":"model-x","prompt_price_per_1m":3,` +
		`"completion_price_per_1m":15,"cache_read_price_per_1m":0.3,` +
		`"cache_write_price_per_1m":3.75,"price_multiplier":1}]}`)
	var envelope pricingEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode KEEPER wire body: %v", err)
	}
	if len(envelope.Pricing) != 1 {
		t.Fatalf("expected 1 pricing row, got %d", len(envelope.Pricing))
	}
	row := envelope.Pricing[0]
	if row.Model != "model-x" || row.PromptPricePer1M != 3 || row.CompletionPricePer1M != 15 ||
		row.CacheReadPricePer1M != 0.3 || row.CacheWritePricePer1M != 3.75 || row.Multiplier != 1 {
		t.Fatalf("KEEPER wire shape drifted, decoded row = %+v", row)
	}
}

// TestPriceCacheSyncedSnapshot covers acceptance scenario 3: after a sync,
// the cached snapshot contains the synced price and its sync timestamp is
// the current round's.
func TestPriceCacheSyncedSnapshot(t *testing.T) {
	cache := NewPriceCache()
	source := &fakeSource{prices: []Price{
		{Model: "model-x", PromptPricePer1M: 3, CompletionPricePer1M: 15, Multiplier: 1},
	}}
	syncer := NewSyncer(cache, source, SyncerOptions{})

	syncedAt := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	syncer.SetNow(func() time.Time { return syncedAt })

	if err := syncer.SyncOnce(context.Background()); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}

	prices, at := cache.Get()
	if at != syncedAt {
		t.Fatalf("sync timestamp = %v, want the current round's %v", at, syncedAt)
	}
	row, ok := prices["model-x"]
	if !ok {
		t.Fatalf("synced model missing from the snapshot")
	}
	if row.PromptPricePer1M != 3 || row.CompletionPricePer1M != 15 {
		t.Fatalf("synced price row wrong: %+v", row)
	}
}

// TestPriceCacheConcurrentReads verifies that readers of the cache, running
// concurrently with a stream of whole-table swaps, never observe a torn or
// mixed-generation snapshot (acceptance scenario 3's "无撕裂读"). A
// generation G of the source table holds exactly G+1 models, each stamped
// with its generation; a complete read therefore shows a single
// generation prefix and a matching count. A torn or in-place-mutated read
// would mix prefixes or break the count and is reported.
func TestPriceCacheConcurrentReads(t *testing.T) {
	cache := NewPriceCache()
	source := &dynamicSource{}

	// The writer keeps swapping whole generations in, so readers contend
	// with Apply. (The daily Syncer ticker is not used here: its 24h
	// period never fires inside a test, so a dedicated writer drives the
	// swaps instead.)
	var writer sync.WaitGroup
	stop := make(chan struct{})
	writer.Add(1)
	go func() {
		defer writer.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			prices, _ := source.Fetch(context.Background())
			cache.Apply(prices, time.Now())
		}
	}()

	const readers = 8
	const reads = 2000
	var readWG sync.WaitGroup
	for r := 0; r < readers; r++ {
		readWG.Add(1)
		go func() {
			defer readWG.Done()
			for i := 0; i < reads; i++ {
				prices, _ := cache.Get()
				if len(prices) == 0 {
					continue // the very first read may precede the first swap
				}
				gen := len(prices) - 1 // generation G holds G+1 models
				for key := range prices {
					if !strings.HasPrefix(key, fmt.Sprintf("gen%d-", gen)) {
						t.Errorf("torn read: key %q mixes generations (expected gen%d-*)", key, gen)
						return
					}
				}
			}
		}()
	}
	readWG.Wait()
	close(stop)
	writer.Wait()
}

// dynamicSource serves one growing generation per Fetch call so that every
// cached table is self-describing (see TestPriceCacheConcurrentReads).
type dynamicSource struct {
	mu  sync.Mutex
	gen int
}

func (d *dynamicSource) Fetch(context.Context) ([]Price, error) {
	d.mu.Lock()
	g := d.gen
	d.gen++
	d.mu.Unlock()
	out := make([]Price, g+1)
	for i := range out {
		out[i] = Price{Model: fmt.Sprintf("gen%d-%d", g, i), Multiplier: 1}
	}
	return out, nil
}

// TestPriceCacheStale verifies readers mark a snapshot stale once it is
// older than the horizon.
func TestPriceCacheStale(t *testing.T) {
	cache := NewPriceCache()
	old := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	horizon := 24 * time.Hour

	cache.Apply(nil, old)
	if !cache.IsStale(recent, horizon) {
		t.Fatal("a snapshot older than the horizon must be stale")
	}
	cache.Apply(nil, recent)
	if cache.IsStale(recent.Add(time.Minute), horizon) {
		t.Fatal("a fresh snapshot must not be stale")
	}
}

// TestPriceCachePriceLookup covers the per-model Price lookup and the
// named-result staleness reporting.
func TestPriceCachePriceLookup(t *testing.T) {
	cache := NewPriceCache()
	cache.Apply([]Price{{Model: "m-1", PromptPricePer1M: 3, Multiplier: 1}}, time.Unix(1000, 0))
	horizon := 24 * time.Hour

	info := cache.Price("m-1", time.Unix(2000, 0), horizon)
	if !info.Found || info.Price.PromptPricePer1M != 3 || info.Stale {
		t.Fatalf("fresh lookup = %+v, want found m-1 @ 3, not stale", info)
	}
	// A model the snapshot does not price is unfound, not an error.
	if got := cache.Price("nope", time.Unix(2000, 0), horizon); got.Found {
		t.Fatalf("absent model must be unfound, got %+v", got)
	}
	// A stale snapshot reports stale through the named result.
	info = cache.Price("m-1", time.Unix(1000+int64(horizon.Seconds())+1, 0), horizon)
	if !info.Found || !info.Stale {
		t.Fatalf("stale lookup = %+v, want found + stale", info)
	}
	// A nil cache degrades to unfound + stale so the caller falls back to
	// the default strategy.
	var nilCache *PriceCache
	if got := nilCache.Price("m-1", time.Now(), horizon); got.Found || !got.Stale {
		t.Fatalf("nil cache = %+v, want unfound + stale", got)
	}
}

// TestPriceCacheFailedSyncKeepsLast verifies a failed sync does not clear
// the last good snapshot (a KEEPER outage must not wipe routing's price basis).
func TestPriceCacheFailedSyncKeepsLast(t *testing.T) {
	cache := NewPriceCache()
	source := &fakeSource{prices: []Price{{Model: "a", PromptPricePer1M: 1, Multiplier: 1}}}
	syncer := NewSyncer(cache, source, SyncerOptions{})
	syncer.SetNow(func() time.Time { return time.Unix(1000, 0) })
	if err := syncer.SyncOnce(context.Background()); err != nil {
		t.Fatalf("initial sync: %v", err)
	}

	source.err = fmt.Errorf("keeper down")
	if err := syncer.SyncOnce(context.Background()); err == nil {
		t.Fatal("expected the failed sync to report an error")
	}
	prices, _ := cache.Get()
	if _, ok := prices["a"]; !ok {
		t.Fatal("a failed sync must not clear the last good snapshot")
	}
}
