package routeboundary

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestStoreApplyAndGet(t *testing.T) {
	store := NewStore()
	if store.Verified() {
		t.Fatal("new store should not be verified")
	}
	// Unverified store: nothing is returned even before an Apply.
	if _, ok := store.Get("sk-a"); ok {
		t.Fatal("unverified store must not serve boundaries")
	}

	store.Apply([]Boundary{
		{APIKey: "sk-a", Enabled: true, AllowedProviders: []string{"OpenRouter", "anthropic"}, Strict: true},
		{APIKey: "sk-b", Enabled: false, AllowedProviders: []string{"claude"}},
	})
	if !store.Verified() {
		t.Fatal("store should be verified after Apply")
	}
	b, ok := store.Get("sk-a")
	if !ok {
		t.Fatal("expected boundary for sk-a")
	}
	// Providers are normalized on apply.
	if !b.Allows("openrouter") || !b.Allows("ANTHROPIC") {
		t.Fatalf("sk-a boundary should allow normalized providers: %+v", b.AllowedProviders)
	}
	if b.Allows("claude") {
		t.Fatal("sk-a boundary must not allow claude")
	}
	if !b.Constrains() {
		t.Fatal("sk-a should constrain")
	}
	b2, ok := store.Get("sk-b")
	if !ok {
		t.Fatal("expected boundary for sk-b")
	}
	if b2.Constrains() {
		t.Fatal("disabled sk-b must not constrain")
	}
	if _, ok := store.Get("sk-missing"); ok {
		t.Fatal("unexpected boundary for sk-missing")
	}
	if store.Count() != 2 {
		t.Fatalf("expected count 2, got %d", store.Count())
	}
}

func TestStoreApplyReplacesSnapshot(t *testing.T) {
	store := NewStore()
	store.Apply([]Boundary{{APIKey: "sk-a", Enabled: true, AllowedProviders: []string{"claude"}}})
	store.Apply([]Boundary{{APIKey: "sk-b", Enabled: true, AllowedProviders: []string{"gemini"}}})
	if _, ok := store.Get("sk-a"); ok {
		t.Fatal("sk-a should be gone after snapshot replacement")
	}
	if _, ok := store.Get("sk-b"); ok != true {
		t.Fatal("sk-b should be present")
	}
}

// TestStoreConcurrentApplyAndGet checks that a reader never observes a torn
// snapshot while writers apply concurrently: each read must see one whole
// generation (all-or-nothing key sets), never a mix of two.
func TestStoreConcurrentApplyAndGet(t *testing.T) {
	store := NewStore()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(gen int) {
			defer wg.Done()
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				// Generation n of writer gen owns exactly its own keys.
				var rows []Boundary
				for k := 0; k < 5; k++ {
					rows = append(rows, Boundary{
						APIKey:           string(rune('a'+gen)) + string(rune('0'+k)),
						Enabled:          true,
						AllowedProviders: []string{"claude"},
					})
				}
				store.Apply(rows)
				if n%1000 == 0 {
					// Yield so the scheduler actually interleaves readers
					// with writers.
					time.Sleep(time.Microsecond)
				}
			}
		}(i)
	}

	// Readers: a Get for a writer's key sees that writer's whole row shape
	// (APIKey echoed, one allowed provider), never a torn entry. Readers
	// report failures via an atomic flag — t.Fatalf is not safe from a
	// non-test goroutine.
	var torn atomic.Bool
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				b, ok := store.Get("a0")
				if ok && (b.APIKey != "a0" || len(b.AllowedProviders) != 1) {
					torn.Store(true)
					return
				}
			}
		}()
	}

	// Run the stress for a bounded wall-clock window; the point is
	// interleaving, not a deterministic step count.
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
	if torn.Load() {
		t.Fatal("torn snapshot observed under concurrent Apply/Get")
	}
}
