package licenseserver

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	return &Store{
		keysFile:  "test_keys.json",
		stateFile: "test_state.json",
		Keys: map[string]LicenseKeyDef{
			"test-key": {
				MaxActivations: 2,
				ExpiresAt:      time.Now().Add(365 * 24 * time.Hour).UTC().Format(time.RFC3339),
				Metadata:       map[string]string{"tier": "test"},
			},
			"expired-key": {
				MaxActivations: 1,
				ExpiresAt:      time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
			},
		},
		Activations: map[string]map[string]ActivationRecord{},
	}
}

func serve(store *Store) *httptest.Server {
	svr := NewServer(store, time.Hour)
	return httptest.NewServer(svr.Handler())
}

func postJSON(t *testing.T, url string, payload any) (*http.Response, map[string]any) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	var decoded map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp, decoded
}

func TestActivateAndHeartbeat(t *testing.T) {
	store := newTestStore(t)
	ts := serve(store)
	defer ts.Close()

	resp, body := postJSON(t, ts.URL+"/activate", ActivateRequest{LicenseKey: "test-key", MachineID: "machine-a"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("activate status = %d, body=%v", resp.StatusCode, body)
	}
	if body["valid"] != true {
		t.Fatalf("expected valid=true, got %v", body)
	}

	// Duplicate activation from same machine is fine.
	resp, _ = postJSON(t, ts.URL+"/activate", ActivateRequest{LicenseKey: "test-key", MachineID: "machine-a"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("re-activate status = %d", resp.StatusCode)
	}

	// Heartbeat renews the lease.
	resp, body = postJSON(t, ts.URL+"/heartbeat", HeartbeatRequest{LicenseKey: "test-key", MachineID: "machine-a"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("heartbeat status = %d", resp.StatusCode)
	}
	if body["valid"] != true {
		t.Fatalf("expected heartbeat valid, got %v", body)
	}

	// Status reports active.
	resp2, err := http.Get(ts.URL + "/status?license_key=test-key")
	if err != nil {
		t.Fatalf("status get: %v", err)
	}
	defer resp2.Body.Close()
	var status StatusResponse
	if err := json.NewDecoder(resp2.Body).Decode(&status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if !status.Valid || status.Status != "active" {
		t.Fatalf("expected active, got %+v", status)
	}
}

func TestInvalidAndExpiredKeys(t *testing.T) {
	store := newTestStore(t)
	ts := serve(store)
	defer ts.Close()

	// Unknown key.
	resp, body := postJSON(t, ts.URL+"/activate", ActivateRequest{LicenseKey: "nope", MachineID: "m"})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown key status = %d, body=%v", resp.StatusCode, body)
	}

	// Expired key.
	resp, body = postJSON(t, ts.URL+"/activate", ActivateRequest{LicenseKey: "expired-key", MachineID: "m"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expired key status = %d, body=%v", resp.StatusCode, body)
	}
}

func TestMaxActivations(t *testing.T) {
	store := newTestStore(t)
	ts := serve(store)
	defer ts.Close()

	// Key allows 2 activations; third distinct machine is rejected.
	for _, m := range []string{"m1", "m2", "m3"} {
		resp, body := postJSON(t, ts.URL+"/activate", ActivateRequest{LicenseKey: "test-key", MachineID: m})
		if m == "m3" {
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("third machine status = %d, body=%v", resp.StatusCode, body)
			}
		} else if resp.StatusCode != http.StatusOK {
			t.Fatalf("machine %s status = %d", m, resp.StatusCode)
		}
	}
}

func TestMethodNotAllowed(t *testing.T) {
	store := newTestStore(t)
	ts := serve(store)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/activate")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", resp.StatusCode)
	}
}

func TestEvaluateOfflineGrace(t *testing.T) {
	grace := 7 * 24 * time.Hour

	// Within grace.
	status := EvaluateOfflineGrace(time.Now().Add(-time.Hour), grace)
	if status.Mode != "grace" {
		t.Fatalf("expected grace, got %s", status.Mode)
	}

	// Grace exhausted.
	status = EvaluateOfflineGrace(time.Now().Add(-8*24*time.Hour), grace)
	if status.Mode != "expired" {
		t.Fatalf("expected expired, got %s", status.Mode)
	}

	// No prior success.
	status = EvaluateOfflineGrace(time.Time{}, grace)
	if status.Mode != "expired" {
		t.Fatalf("expected expired for zero last success, got %s", status.Mode)
	}

	// Grace disabled (<= 0).
	status = EvaluateOfflineGrace(time.Now(), 0)
	if status.Mode != "expired" {
		t.Fatalf("expected expired for zero grace, got %s", status.Mode)
	}
}

func TestStorePersistence(t *testing.T) {
	dir := t.TempDir()
	keysFile := dir + "/keys.json"
	stateFile := dir + "/state.json"

	keysData, _ := json.Marshal(map[string]map[string]LicenseKeyDef{"keys": {"persist-key": {MaxActivations: 1, ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}}})
	if err := writeFile(keysFile, keysData); err != nil {
		t.Fatalf("write keys: %v", err)
	}

	store, err := NewStore(keysFile, stateFile)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	if _, err := store.Activate("persist-key", "m1", time.Hour); err != nil {
		t.Fatalf("activate: %v", err)
	}

	// Re-open store and confirm activations survive restart.
	reopened, err := NewStore(keysFile, stateFile)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	rec, err := reopened.Status("persist-key")
	if err != nil {
		t.Fatalf("status after reopen: %v", err)
	}
	if rec.Status != "active" || rec.MachineID != "m1" {
		t.Fatalf("unexpected record after reopen: %+v", rec)
	}
}

func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0644)
}