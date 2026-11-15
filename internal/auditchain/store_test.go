package auditchain

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func testStore(t *testing.T, nowFn func() time.Time) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit.db")
	s, err := NewStore(path, nowFn)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

var testClock = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

// openRaw re-opens the database for direct row-level tamper in a test.
func openRaw(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("re-open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// Scenario 1: query chain by operation ID returns the full timeline in
// chronological order with the approver, the model summary, the tool
// arguments and the execution result.
func TestQueryChainByOperationID(t *testing.T) {
	now := testClock
	s, _ := testStore(t, func() time.Time { return now.Add(time.Second) })

	op := "op-del-001"
	payloadJSON, _ := json.Marshal(map[string]any{"tool": "delete_data", "args": map[string]string{"table": "customers"}})

	_, err := s.Append(context.Background(), Event{
		OperationID: op, Kind: KindModelDecision,
		Summary:    "model suggests deleting the customers table (95% confidence)",
		OccurredAt: now,
	}, nil)
	if err != nil {
		t.Fatalf("append decision: %v", err)
	}
	_, err = s.Append(context.Background(), Event{
		OperationID: op, Kind: KindApproval, Actor: "alice@corp",
		Summary:    "approved after risk review",
		OccurredAt: now.Add(time.Minute),
	}, nil)
	if err != nil {
		t.Fatalf("append approval: %v", err)
	}
	_, err = s.Append(context.Background(), Event{
		OperationID: op, Kind: KindToolCall,
		Summary:     "delete_data invoked",
		PayloadJSON: string(payloadJSON),
		OccurredAt:  now.Add(2 * time.Minute),
	}, nil)
	if err != nil {
		t.Fatalf("append tool_call: %v", err)
	}
	_, err = s.Append(context.Background(), Event{
		OperationID: op, Kind: KindResult,
		Summary:    "312 rows deleted",
		OccurredAt: now.Add(3 * time.Minute),
	}, nil)
	if err != nil {
		t.Fatalf("append result: %v", err)
	}

	chain, err := s.Read(context.Background(), op, time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(chain.Events) != 4 {
		t.Fatalf("expected 4 events, got %d", len(chain.Events))
	}
	if !chain.Integrity.Intact || chain.Integrity.Tampered {
		t.Fatalf("expected intact chain, got %+v", chain.Integrity)
	}
	for i := 1; i < len(chain.Events); i++ {
		if chain.Events[i].OccurredAt.Before(chain.Events[i-1].OccurredAt) {
			t.Fatalf("events not in chronological order at seq %d", chain.Events[i].Seq)
		}
	}
	var sawApprover, sawModelSummary, sawToolArgs, sawResult bool
	var approvalTime time.Time
	for _, ev := range chain.Events {
		switch ev.Kind {
		case KindApproval:
			if ev.Actor != "alice@corp" {
				t.Fatalf("approver = %q, want alice@corp", ev.Actor)
			}
			approvalTime = ev.OccurredAt
			sawApprover = true
		case KindModelDecision:
			if ev.Summary == "" {
				t.Fatalf("model decision summary empty")
			}
			sawModelSummary = true
		case KindToolCall:
			var p map[string]any
			if err := json.Unmarshal([]byte(ev.PayloadJSON), &p); err != nil {
				t.Fatalf("tool call args: %v", err)
			}
			if p["tool"] != "delete_data" {
				t.Fatalf("tool = %v, want delete_data", p["tool"])
			}
			sawToolArgs = true
		case KindResult:
			if ev.Summary == "" {
				t.Fatalf("result summary empty")
			}
			sawResult = true
		}
	}
	if !sawApprover || !sawModelSummary || !sawToolArgs || !sawResult {
		t.Fatalf("missing required fields: approver=%v model=%v args=%v result=%v",
			sawApprover, sawModelSummary, sawToolArgs, sawResult)
	}
	if approvalTime != now.Add(time.Minute) {
		t.Fatalf("approval time = %v, want %v", approvalTime, now.Add(time.Minute))
	}
}

// Scenario 3 (tamper detection): modify a stored row in place; the next
// read must flag the chain as tampered and report the intact prefix
// digest.
func TestTamperedChainIsDetected(t *testing.T) {
	now := testClock
	s, path := testStore(t, func() time.Time { return now })
	op := "op-tamper-002"
	for i := 0; i < 3; i++ {
		if _, err := s.Append(context.Background(), Event{
			OperationID: op, Kind: KindModelDecision,
			Summary:    "decision " + string(rune('a'+i)),
			OccurredAt: now.Add(time.Duration(i) * time.Minute),
		}, nil); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	// Record the intact prefix digest (seq 1) BEFORE tampering so the
	// assertion below checks the real value, not a tautology.
	goodChain, err := s.Read(context.Background(), op, time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatalf("read good: %v", err)
	}
	var seq1Hash string
	for _, ev := range goodChain.Events {
		if ev.Seq == 1 {
			seq1Hash = ev.Hash
		}
	}
	if seq1Hash == "" {
		t.Fatalf("seq 1 hash not found in good chain")
	}

	// Flip a record in the raw database: its digest must no longer match
	// the recomputation.
	db := openRaw(t, path)
	if _, err := db.Exec(`UPDATE audit_chain_events SET summary = 'forged' WHERE operation_id = ? AND seq = 2`, op); err != nil {
		t.Fatalf("tamper row: %v", err)
	}

	chain, err := s.Read(context.Background(), op, time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !chain.Integrity.Tampered || chain.Integrity.FirstBadSeq != 2 {
		t.Fatalf("expected tamper at seq 2, got %+v", chain.Integrity)
	}
	// The intact prefix digest must be exactly seq 1's hash, not empty and
	// not the last record's.
	if chain.Integrity.HeadHash != seq1Hash {
		t.Fatalf("intact prefix digest = %s, want the seq-1 hash %s", chain.Integrity.HeadHash, seq1Hash)
	}
}

// The approver's identity is bound into the chain hash; rewriting the
// actor field after the fact must break integrity — the "approval
// identity cannot be forged" guarantee.
func TestApproverIdentityIsHashBound(t *testing.T) {
	now := testClock
	s, path := testStore(t, func() time.Time { return now })
	op := "op-actor-003"
	if _, err := s.Append(context.Background(), Event{
		OperationID: op, Kind: KindApproval, Actor: "bob@corp",
		Summary:    "approved",
		OccurredAt: now,
	}, nil); err != nil {
		t.Fatalf("append: %v", err)
	}
	db := openRaw(t, path)
	if _, err := db.Exec(`UPDATE audit_chain_events SET actor = 'mallory@evil' WHERE operation_id = ? AND seq = 1`, op); err != nil {
		t.Fatalf("rewrite actor: %v", err)
	}
	chain, err := s.Read(context.Background(), op, time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !chain.Integrity.Tampered || chain.Integrity.FirstBadSeq != 1 {
		t.Fatalf("rewriting the approver must break the chain, got %+v", chain.Integrity)
	}
}

// Hash-ambiguity regression: moving a '|' between kind and actor must not
// produce a second valid encoding of the record.
func TestHashIsUnambiguousAcrossFields(t *testing.T) {
	h1 := eventHash(StoredEvent{Seq: 1, Kind: "approval", Actor: "alice|ceo", PrevHash: "g"}, "op", nil)
	h2 := eventHash(StoredEvent{Seq: 1, Kind: "approval|alice", Actor: "ceo", PrevHash: "g"}, "op", nil)
	if h1 == h2 {
		t.Fatalf("field-shift collision: kind|actor ambiguous in the record hash")
	}
}

// seq is bound into the hash: a single record cannot be relabelled to a
// higher seq without breaking verification.
func TestSeqIsHashBound(t *testing.T) {
	now := testClock
	s, path := testStore(t, func() time.Time { return now })
	op := "op-seq-009"
	if _, err := s.Append(context.Background(), Event{
		OperationID: op, Kind: KindResult, Summary: "step", OccurredAt: now,
	}, nil); err != nil {
		t.Fatalf("append: %v", err)
	}
	db := openRaw(t, path)
	// Relabel the only record to seq 7; the genesis link and seq
	// contiguity must both fail.
	if _, err := db.Exec(`UPDATE audit_chain_events SET seq = 7 WHERE operation_id = ? AND seq = 1`, op); err != nil {
		t.Fatalf("relabel seq: %v", err)
	}
	chain, err := s.Read(context.Background(), op, time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !chain.Integrity.Tampered || chain.Integrity.FirstBadSeq != 7 {
		t.Fatalf("relabelled seq must break the chain, got %+v", chain.Integrity)
	}
}

// Out-of-range event times are rejected at the append boundary, so the
// stored (and hash-bound) timestamp can never silently wrap.
func TestOccurredAtRangeIsValidated(t *testing.T) {
	s, _ := testStore(t, func() time.Time { return testClock })
	if _, err := s.Append(context.Background(), Event{
		OperationID: "op-time-010", Kind: KindResult,
		OccurredAt: time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC),
	}, nil); err == nil {
		t.Fatalf("year-3000 event time must be rejected")
	}
	if _, err := s.Append(context.Background(), Event{
		OperationID: "op-time-010", Kind: KindResult,
		OccurredAt: time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC),
	}, nil); err == nil {
		t.Fatalf("year-1900 event time must be rejected")
	}
}

// An approval event without an actor is rejected: the approver identity is
// a non-reducible contract field.
func TestApprovalRequiresActor(t *testing.T) {
	s, _ := testStore(t, func() time.Time { return testClock })
	_, err := s.Append(context.Background(), Event{
		OperationID: "op-noactor",
		Kind:        KindApproval,
		Summary:     "approved",
		OccurredAt:  testClock,
	}, nil)
	if err == nil {
		t.Fatalf("approval without actor must be rejected")
	}
	if !strings.Contains(err.Error(), "actor") {
		t.Fatalf("error should mention the actor requirement, got %v", err)
	}
}

// An unknown operation read returns ErrOperationNotFound, not an empty
// chain.
func TestReadUnknownOperation(t *testing.T) {
	s, _ := testStore(t, func() time.Time { return testClock })
	_, err := s.Read(context.Background(), "never-started", time.Time{}, time.Time{}, 0)
	if !errors.Is(err, ErrOperationNotFound) {
		t.Fatalf("expected ErrOperationNotFound, got %v", err)
	}
}

// The required time-range filter returns only the events inside the window
// while integrity is still verified across the full sequence.
func TestTimeRangeFilter(t *testing.T) {
	now := testClock
	s, path := testStore(t, func() time.Time { return now })
	op := "op-window-004"
	for i := 0; i < 5; i++ {
		if _, err := s.Append(context.Background(), Event{
			OperationID: op, Kind: KindResult,
			Summary:    "step",
			OccurredAt: now.Add(time.Duration(i) * time.Minute),
		}, nil); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	from := now.Add(1 * time.Minute)
	to := now.Add(3 * time.Minute)
	chain, err := s.Read(context.Background(), op, from, to, 0)
	if err != nil {
		t.Fatalf("read window: %v", err)
	}
	if len(chain.Events) != 3 {
		t.Fatalf("expected 3 events in [1m,3m], got %d", len(chain.Events))
	}
	if !chain.Integrity.Intact {
		t.Fatalf("intact chain must verify even with a time window: %+v", chain.Integrity)
	}

	// The window must not mask a tamper in a record outside the window:
	// tamper seq 1 (the 0m event, outside [1m,3m]) and re-read the
	// window — the verdict must still report the broken chain.
	db := openRaw(t, path)
	if _, err := db.Exec(`UPDATE audit_chain_events SET summary = 'forged' WHERE operation_id = ? AND seq = 1`, op); err != nil {
		t.Fatalf("tamper outside window: %v", err)
	}
	wCh, err := s.Read(context.Background(), op, from, to, 0)
	if err != nil {
		t.Fatalf("read window after tamper: %v", err)
	}
	if !wCh.Integrity.Tampered || wCh.Integrity.FirstBadSeq != 1 {
		t.Fatalf("a tamper outside the read window must still be reported: %+v", wCh.Integrity)
	}
}

// Snapshot capture is fail-closed: without a key the fields are dropped
// (never stored in plaintext) and the record reports that.
func TestSnapshotFailsClosedWithoutKey(t *testing.T) {
	now := testClock
	s, path := testStore(t, func() time.Time { return now })
	op := "op-snap-005"
	row, err := s.Append(context.Background(), Event{
		OperationID: op,
		Kind:        KindModelDecision,
		Summary:     "model suggestion",
		ModelInput:  "secret user prompt",
		OccurredAt:  now,
	}, nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if row.SnapshotRecorded {
		t.Fatalf("no key configured: snapshot must not be recorded")
	}
	chain, err := s.Read(context.Background(), op, time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(chain.Events) != 1 || chain.Events[0].SnapshotRecorded {
		t.Fatalf("record must report no snapshot without a key: %+v", chain.Events)
	}
	// Prove the plaintext was never persisted anywhere in the database
	// file.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read db file: %v", err)
	}
	if strings.Contains(string(raw), "secret user prompt") {
		t.Fatalf("model context must not be stored in plaintext without a key")
	}
}

// With a key the model context is captured encrypted and the chain stays
// intact; an attacker changing the encrypted blob must be detected.
func TestSnapshotEncryptedAndTamperEvident(t *testing.T) {
	key, _ := hex.DecodeString(strings.Repeat("ab", 32))
	now := testClock
	s, path := testStore(t, func() time.Time { return now })
	op := "op-snap-006"
	row, err := s.Append(context.Background(), Event{
		OperationID: op, Kind: KindModelDecision,
		Summary:    "model suggestion",
		ModelInput: "secret user prompt",
		OccurredAt: now,
	}, key)
	if err != nil {
		t.Fatalf("append with key: %v", err)
	}
	if !row.SnapshotRecorded {
		t.Fatalf("with a key the snapshot must be recorded")
	}
	db := openRaw(t, path)
	var blob []byte
	if err := db.QueryRow(`SELECT snapshot FROM audit_chain_events WHERE operation_id = ? AND seq = 1`, op).Scan(&blob); err != nil {
		t.Fatalf("read snapshot blob: %v", err)
	}
	if strings.Contains(string(blob), "secret user prompt") {
		t.Fatalf("snapshot must not store the plaintext model input")
	}

	chain, err := s.Read(context.Background(), op, time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !chain.Integrity.Intact {
		t.Fatalf("encrypted chain must verify intact: %+v", chain.Integrity)
	}

	// Tamper the encrypted blob: the record hash must now fail.
	if _, err := db.Exec(`UPDATE audit_chain_events SET snapshot = x'deadbeef' WHERE operation_id = ? AND seq = 1`, op); err != nil {
		t.Fatalf("tamper blob: %v", err)
	}
	chain, err = s.Read(context.Background(), op, time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatalf("read after tamper: %v", err)
	}
	if !chain.Integrity.Tampered {
		t.Fatalf("swapping the encrypted snapshot must be detected: %+v", chain.Integrity)
	}
}

// Concurrent appends to the same operation must all succeed with unique,
// contiguous seqs — evidence must not be lost under write contention.
func TestConcurrentAppendsSerialize(t *testing.T) {
	now := testClock
	s, _ := testStore(t, func() time.Time { return now })
	op := "op-conc-011"

	const workers = 8
	var wg sync.WaitGroup
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, err := s.Append(context.Background(), Event{
				OperationID: op, Kind: KindResult,
				Summary:    "concurrent " + string(rune('A'+n)),
				OccurredAt: now.Add(time.Duration(n) * time.Millisecond),
			}, nil)
			errs[n] = err
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent append %d failed: %v", i, err)
		}
	}
	chain, err := s.Read(context.Background(), op, time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(chain.Events) != workers {
		t.Fatalf("expected %d records, got %d (lost evidence under concurrency)", workers, len(chain.Events))
	}
	for i, ev := range chain.Events {
		if ev.Seq != int64(i+1) {
			t.Fatalf("seq not contiguous at position %d: got %d", i, ev.Seq)
		}
	}
	if !chain.Integrity.Intact {
		t.Fatalf("concurrent chain must verify intact: %+v", chain.Integrity)
	}
}

// The management-auth boundary: when the channel handler is present, the
// routes are mounted inside the authenticated group. This test asserts the
// 404 "channel disabled" path and the handler method dispatch, the
// server-side half of scenario 2 (no permission, no content).
func TestHandlerMethodDispatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s, _ := testStore(t, func() time.Time { return testClock })
	h := NewHandler(s, nil)

	body := `{"operation_id":"op-http-007","kind":"result","summary":"done"}`
	req := httptest.NewRequest(http.MethodPost, "/v0/management/audit/events", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rr)
	ginCtx.Request = req
	ginCtx.Params = gin.Params{}
	h.Handle(ginCtx)
	if rr.Code != http.StatusCreated {
		t.Fatalf("POST /audit/events = %d, want 201 (body: %s)", rr.Code, rr.Body.String())
	}

	reqGet := httptest.NewRequest(http.MethodGet, "/v0/management/audit/operations/op-http-007", nil)
	rrGet := httptest.NewRecorder()
	ginCtxGet, _ := gin.CreateTestContext(rrGet)
	ginCtxGet.Request = reqGet
	ginCtxGet.Params = gin.Params{{Key: "id", Value: "op-http-007"}}
	h.Handle(ginCtxGet)
	if rrGet.Code != http.StatusOK {
		t.Fatalf("GET /audit/operations/op-http-007 = %d, want 200 (body: %s)", rrGet.Code, rrGet.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rrGet.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if resp["operation_id"] != "op-http-007" {
		t.Fatalf("operation_id = %v, want op-http-007", resp["operation_id"])
	}

	reqMiss := httptest.NewRequest(http.MethodGet, "/v0/management/audit/operations/none", nil)
	rrMiss := httptest.NewRecorder()
	ginCtxMiss, _ := gin.CreateTestContext(rrMiss)
	ginCtxMiss.Request = reqMiss
	ginCtxMiss.Params = gin.Params{{Key: "id", Value: "none"}}
	h.Handle(ginCtxMiss)
	if rrMiss.Code != http.StatusNotFound {
		t.Fatalf("GET unknown operation = %d, want 404", rrMiss.Code)
	}

	// An oversized body is rejected before the field caps would matter.
	big := strings.Repeat("a", maxBodyBytes+1)
	reqBig := httptest.NewRequest(http.MethodPost, "/v0/management/audit/events", strings.NewReader(big))
	rrBig := httptest.NewRecorder()
	ginCtxBig, _ := gin.CreateTestContext(rrBig)
	ginCtxBig.Request = reqBig
	ginCtxBig.Params = gin.Params{}
	h.Handle(ginCtxBig)
	if rrBig.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body = %d, want 413", rrBig.Code)
	}

	// An invalid operation id is a 400, not a 500.
	reqBad := httptest.NewRequest(http.MethodGet, "/v0/management/audit/operations/bad%20id!", nil)
	rrBad := httptest.NewRecorder()
	ginCtxBad, _ := gin.CreateTestContext(rrBad)
	ginCtxBad.Request = reqBad
	ginCtxBad.Params = gin.Params{{Key: "id", Value: "bad id!"}}
	h.Handle(ginCtxBad)
	if rrBad.Code != http.StatusBadRequest {
		t.Fatalf("invalid operation id = %d, want 400", rrBad.Code)
	}
}

// A disabled channel (nil handler) is the 404 path the api package serves.
func TestHandlerNilStoreReportsUnavailable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewHandler(nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/v0/management/audit/operations/x", nil)
	rr := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rr)
	ginCtx.Request = req
	ginCtx.Params = gin.Params{{Key: "id", Value: "x"}}
	h.Handle(ginCtx)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("nil store must report 404, got %d", rr.Code)
	}
}

// Invalid hex key config is fail-closed: capture disabled, not plaintext.
func TestMalformedKeyIsFailClosed(t *testing.T) {
	t.Setenv(EnvKeyHex, "zz")
	if keyFromEnv() != nil {
		t.Fatalf("malformed hex key must return nil (capture disabled)")
	}
}

// ServerOption returns nil when the channel is disabled by default.
func TestServerOptionDisabledByDefault(t *testing.T) {
	t.Setenv(EnvEnabled, "")
	if opt := ServerOption(); opt != nil {
		t.Fatalf("ServerOption must be nil when the channel is off by default")
	}
}

// ServerOption opens the database and returns an option when enabled.
func TestServerOptionEnabled(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv(EnvEnabled, "true")
	t.Setenv(EnvDBPath, filepath.Join(tmp, "audit.db"))
	if opt := ServerOption(); opt == nil {
		t.Fatalf("ServerOption must be non-nil when enabled and the db is openable")
	}
	info, err := os.Stat(filepath.Join(tmp, "audit.db"))
	if err != nil {
		t.Fatalf("audit db should exist: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("audit db must be 0600, got %o", info.Mode().Perm())
	}
}
