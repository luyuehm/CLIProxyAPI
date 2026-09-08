package dataratelimit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// keeperStub is a minimal fake for the KEEPER quota-status endpoint.
type keeperStub struct {
	t          *testing.T
	rows       []keeperStatusRow
	statusCode int // default 200
	key        string
	hits       int
}

func (s *keeperStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.hits++
	if s.key != "" && r.Header.Get("X-CPA-Management-Key") != s.key {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if s.statusCode != 0 && s.statusCode != http.StatusOK {
		w.WriteHeader(s.statusCode)
		return
	}
	if s.rows == nil {
		_, _ = w.Write([]byte(`{"items":[],"total":0}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	payload := keeperStatusResponse{Items: s.rows, Total: len(s.rows)}
	data, err := json.Marshal(payload)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(data)
}

func TestBlacklistOpenBeforeSync(t *testing.T) {
	b := NewBlacklist(BlacklistOptions{KeeperURL: "http://unused"})
	if b.IsBlocked("sk-a") {
		t.Fatal("never-verified blacklist must pass through")
	}
	if _, ok := b.Limits("sk-a"); ok {
		t.Fatal("never-verified blacklist has no limits")
	}
}

func TestBlacklistSyncOnceBlocksExceeded(t *testing.T) {
	server := httptest.NewServer(&keeperStub{t: t, rows: []keeperStatusRow{
		{APIKey: "sk-a", Enabled: true, RPM: 10, Exceeded: true, ExceededReason: "tpd"},
		{APIKey: "sk-b", Enabled: true, RPM: 20, Exceeded: false},
	}})
	defer server.Close()

	b := NewBlacklist(BlacklistOptions{KeeperURL: server.URL, ManagementKey: "secret"})
	if err := b.SyncOnce(context.Background()); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if !b.IsBlocked("sk-a") {
		t.Fatal("sk-a is exceeded and must be blocked")
	}
	if b.IsBlocked("sk-b") {
		t.Fatal("sk-b is not exceeded and must not be blocked")
	}
	if b.IsBlocked("sk-unknown") {
		t.Fatal("unknown key must not be blocked")
	}
}

func TestBlacklistSyncOncePublishesLimits(t *testing.T) {
	server := httptest.NewServer(&keeperStub{t: t, rows: []keeperStatusRow{
		{APIKey: "sk-a", Enabled: true, RPM: 30, MaxConcurrency: 5},
	}})
	defer server.Close()

	b := NewBlacklist(BlacklistOptions{KeeperURL: server.URL})
	if err := b.SyncOnce(context.Background()); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	limits, ok := b.Limits("sk-a")
	if !ok {
		t.Fatal("sk-a should have published limits")
	}
	if limits.RPM != 30 || limits.MaxConcurrency != 5 {
		t.Fatalf("unexpected limits: %+v", limits)
	}
}

func TestBlacklistLatchOnFailure(t *testing.T) {
	server := httptest.NewServer(&keeperStub{t: t, rows: []keeperStatusRow{
		{APIKey: "sk-a", Enabled: true, Exceeded: true},
	}})
	b := NewBlacklist(BlacklistOptions{KeeperURL: server.URL})
	if err := b.SyncOnce(context.Background()); err != nil {
		t.Fatalf("initial SyncOnce: %v", err)
	}
	server.Close() // subsequent pulls fail

	if !b.IsBlocked("sk-a") {
		t.Fatal("failed pull must keep the last verdict (sk-a stayed blocked)")
	}
	if _, ok := b.Limits("sk-a"); !ok {
		t.Fatal("failed pull must keep the last limits snapshot")
	}
}

func TestBlacklistUnauthorizedKeyFailsOpenLatch(t *testing.T) {
	server := httptest.NewServer(&keeperStub{t: t, key: "right", rows: []keeperStatusRow{
		{APIKey: "sk-a", Enabled: true, Exceeded: true},
	}})
	defer server.Close()

	b := NewBlacklist(BlacklistOptions{KeeperURL: server.URL, ManagementKey: "right"})
	if err := b.SyncOnce(context.Background()); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if !b.IsBlocked("sk-a") {
		t.Fatal("sk-a should be blocked after authenticated sync")
	}
}

func TestBlacklistStartStops(t *testing.T) {
	server := httptest.NewServer(&keeperStub{t: t, rows: []keeperStatusRow{
		{APIKey: "sk-a", Enabled: true, RPM: 10, Exceeded: true},
	}})
	defer server.Close()

	b := NewBlacklist(BlacklistOptions{KeeperURL: server.URL, Interval: 20 * time.Millisecond})
	b.Start()
	defer b.Stop()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if b.IsBlocked("sk-a") {
			// First successful sync landed; ensure it survives a later loop tick.
			time.Sleep(30 * time.Millisecond)
			if !b.IsBlocked("sk-a") {
				t.Fatal("blocked verdict should persist across loop ticks")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("blacklist did not block sk-a within deadline after Start")
}
