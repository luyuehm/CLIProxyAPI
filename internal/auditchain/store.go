// Package auditchain implements the RIC-2686 chain of responsibility: an
// append-only, hash-chained evidence log for high-risk automated actions.
//
// Each business operation (identified by a business operation ID) accumulates
// events in four kinds — model_decision, approval, tool_call and result —
// linking the model's recommendation context, the human approver's identity
// and decision, the actual tool invocation and the execution outcome into
// one tamper-evident timeline. Read paths re-verify the hash chain and mark
// it tampered (with a digest of the intact prefix) when any record has been
// modified.
//
// The channel is env-gated and off by default, matching the sibling
// KEEPER-facing channels (contentfilter / policies / arbitrage): an
// unconfigured build mounts no routes and adds no goroutines. The HTTP
// surface is registered inside the authenticated /v0/management group via
// the reserved handler injection points in internal/api, so every request
// is rejected with 401/403 (and no record content) before it reaches the
// handlers.
//
// Threat model: the chain is tamper-*evident* for a reader that cannot write
// the database — any in-place rewrite of a record (summary, actor, payload,
// kind, timestamp, snapshot) breaks the record's digest and is reported as
// tampered on the next read. A party that can write the database can recompute
// a re-anchored tail, so the digest of the current head should be anchored
// externally (e.g. the KEEPER control plane) for a hard guarantee; that is a
// follow-up, out of scope here.
package auditchain

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Event kinds recorded in the chain. Unknown kinds are accepted with the
// same validation rules; the four below are the contract of the
// chain-of-responsibility timeline.
const (
	KindModelDecision = "model_decision"
	KindApproval      = "approval"
	KindToolCall      = "tool_call"
	KindResult        = "result"
)

// AuthMethodManagement marks events ingested through the authenticated
// management plane. The identity of the approver is bound into the hash
// chain, so a later modification of the actor field is detectable.
const AuthMethodManagement = "management_key"

const (
	// schemaVersion is embedded in every hash so a re-key or format change
	// produces a fresh genesis rather than silently reusing old digests.
	schemaVersion = "audit-chain-v1"

	// hashVersion is bound into the record digest; bump it when the
	// encoding of hashInput changes so old rows verify under the old
	// version only.
	hashVersion = 1

	maxOperationIDLen = 128
	maxKindLen        = 32
	maxActorLen       = 256
	maxSummaryLen     = 4096
	// maxPayloadBytes bounds a single JSON payload; the evidence contract
	// is summary + structured args, not full request bodies.
	maxPayloadBytes = 64 << 10
	// maxSnapshotBytes bounds each of model_input / model_output.
	maxSnapshotBytes = 256 << 10

	// maxReadLimit caps how many events a single read materializes for
	// display. Integrity is still verified over the full sequence; this
	// bounds memory on a very long operation.
	maxReadLimit = 100000
)

// Timestamp bounds for client-supplied occurred_at. Go's UnixNano is only
// defined for 1678–2262; keeping event times well inside that range avoids
// silent wrap-corruption of the stored and hash-bound timestamp.
var (
	minEventTime = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	maxEventTime = time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC)
)

var (
	// ErrOperationNotFound is returned by Read when no event has been
	// recorded for the operation; callers create it implicitly by
	// appending the first event.
	ErrOperationNotFound = errors.New("audit chain: operation not found")
	// ErrBusy is returned by Append when the database is locked and the
	// configured busy timeout elapsed; it is safe to retry.
	ErrBusy = errors.New("audit chain: database is busy")
	// ErrInvalidOperationID is returned for an operation id that fails
	// validation. Callers in the HTTP layer map this to 400.
	ErrInvalidOperationID = errors.New("audit chain: invalid operation id")
	// ErrInvalidKey is returned for a malformed encryption key length.
	ErrInvalidKey = errors.New("audit chain: invalid encryption key")
)

// Event is one record to be appended to the chain of a business operation.
type Event struct {
	// OperationID is the business operation identifier.
	OperationID string
	// Kind is one of the Kind* constants (or a custom kind).
	Kind string
	// Actor is the identity that produced the event. It is required for
	// approval events and bound into the chain hash, so it cannot be
	// rewritten after the fact without breaking integrity.
	Actor string
	// Summary is the human-readable statement of the event: for a
	// model_decision it is the model's recommendation summary; for an
	// approval the approver's decision note; for a tool_call the tool
	// name; for a result the outcome summary.
	Summary string
	// PayloadJSON carries the structured detail (tool call arguments,
	// execution result, model decision metadata). Valid JSON; empty string
	// means "none".
	PayloadJSON string
	// ModelInput / ModelOutput are the model context snapshots. They are
	// recorded only when full context capture is enabled and an
	// encryption key is configured; otherwise they are dropped (privacy
	// fail-closed) and the append reports SnapshotRecorded=false.
	ModelInput  string
	ModelOutput string
	// OccurredAt is the event time; zero means "now".
	OccurredAt time.Time
}

// StoredEvent is an event as persisted in the chain.
type StoredEvent struct {
	Seq              int64
	Kind             string
	Actor            string
	Summary          string
	PayloadJSON      string
	AuthMethod       string
	OccurredAt       time.Time
	PrevHash         string
	Hash             string
	SnapshotRecorded bool
	// snapshot holds the sealed (encrypted) model context capture. It is
	// included in the record hash, so replacing or deleting the capture
	// after the fact breaks the chain.
	snapshot []byte
}

// Integrity is the result of re-verifying one operation's chain.
type Integrity struct {
	// Intact is true when every record hashes to the previous one.
	Intact bool
	// Tampered is the inverse of Intact, spelled out for API readers.
	Tampered bool
	// FirstBadSeq is the seq of the first record whose link or hash does
	// not match the recomputation. Zero when intact.
	FirstBadSeq int64
	// HeadHash is the digest of the last verified record — i.e. the digest
	// of the intact prefix. Anchor it externally for a hard guarantee.
	HeadHash string
}

// OperationChain is the query result for one business operation: the event
// timeline plus the chain-integrity verdict.
type OperationChain struct {
	OperationID string
	Events      []StoredEvent
	Integrity   Integrity
}

// Store persists and verifies the hash chain in one SQLite database.
type Store struct {
	db *sql.DB
	// mu serializes appends so MAX(seq) read and INSERT happen atomically
	// with respect to other in-process appends and no two records can
	// claim the same seq.
	mu sync.Mutex
	// nowFn is injectable for deterministic tests; wall clock in production.
	nowFn func() time.Time
}

// NewStore opens (creating when needed) the SQLite database at path and
// returns a ready store. A freshly created file is 0600 so an audit trail
// is never world-readable.
func NewStore(path string, nowFn func() time.Time) (*Store, error) {
	if nowFn == nil {
		nowFn = func() time.Time { return time.Now().UTC() }
	}
	// Pre-create the file at owner-only permissions; SQLite would open an
	// existing file with its mode intact but create a new one at 0644.
	// umask can mask the create mode, so enforce 0600 explicitly.
	probe, err := os.OpenFile(path, os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("audit chain: create database: %w", err)
	}
	probe.Close()
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("audit chain: chmod database: %w", err)
	}
	// An immediate write lock is taken at BEGIN so the read MAX(seq) and
	// the INSERT share one lock and the deferred read->write upgrade
	// (which SQLite cannot retry under busy_timeout) never happens.
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("audit chain: open database: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("audit chain: open database: %w", err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS audit_chain_events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		operation_id TEXT NOT NULL,
		seq INTEGER NOT NULL,
		kind TEXT NOT NULL,
		actor TEXT NOT NULL DEFAULT '',
		summary TEXT NOT NULL DEFAULT '',
		payload TEXT NOT NULL DEFAULT '',
		snapshot BLOB,
		auth_method TEXT NOT NULL DEFAULT '',
		occurred_at_ns INTEGER NOT NULL,
		prev_hash TEXT NOT NULL,
		hash TEXT NOT NULL,
		created_at TEXT NOT NULL,
		UNIQUE (operation_id, seq)
	)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("audit chain: create schema: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_ac_events_op_seq
		ON audit_chain_events (operation_id, seq)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("audit chain: create index: %w", err)
	}
	return &Store{db: db, nowFn: nowFn}, nil
}

// Close releases the database handle.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Append validates one event, encrypts its snapshots when configured,
// links it to the operation's previous chain hash and writes it
// atomically. The whole append is one immediate write transaction guarded
// by a mutex, so seq allocation and row insertion can never diverge and
// concurrent appends to the same operation cannot collide.
func (s *Store) Append(ctx context.Context, ev Event, key []byte) (StoredEvent, error) {
	// Normalize the operation id so validation and storage agree on the
	// identity (a padded id must not become a different operation).
	ev.OperationID = strings.TrimSpace(ev.OperationID)
	now := s.nowFn()
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = now
	}
	if err := validateEvent(&ev); err != nil {
		return StoredEvent{}, err
	}

	// Snapshot capture: only when a key is available AND the caller
	// actually supplied context. Without a key the fields are dropped
	// (fail-closed) and the caller is told via SnapshotRecorded.
	var snapshot []byte
	snapshotRecorded := false
	if len(key) == 32 && (ev.ModelInput != "" || ev.ModelOutput != "") {
		aead, err := newAEAD(key)
		if err != nil {
			return StoredEvent{}, err
		}
		plain, err := json.Marshal(map[string]string{"model_input": ev.ModelInput, "model_output": ev.ModelOutput})
		if err != nil {
			return StoredEvent{}, fmt.Errorf("audit chain: encode snapshot: %w", err)
		}
		// GCM requires a fresh nonce per seal under the same key.
		nonce := make([]byte, aead.NonceSize())
		if _, err := rand.Read(nonce); err != nil {
			return StoredEvent{}, fmt.Errorf("audit chain: nonce: %w", err)
		}
		snapshot = aead.Seal(nil, nonce, plain, nil)
		snapshotRecorded = true
	}

	row := StoredEvent{
		Kind:             ev.Kind,
		Actor:            ev.Actor,
		Summary:          ev.Summary,
		PayloadJSON:      ev.PayloadJSON,
		AuthMethod:       AuthMethodManagement,
		OccurredAt:       ev.OccurredAt.UTC(),
		SnapshotRecorded: snapshotRecorded,
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return StoredEvent{}, err
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			// Swallow: the original error is returned to the caller.
		}
	}()

	var lastHash string
	err = tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0) FROM audit_chain_events WHERE operation_id = ?`,
		ev.OperationID,
	).Scan(&row.Seq)
	if err != nil {
		return StoredEvent{}, fmt.Errorf("audit chain: read seq: %w", err)
	}
	row.Seq++
	if row.Seq == 1 {
		row.PrevHash = genesisHash(ev.OperationID)
	} else {
		err = tx.QueryRowContext(ctx,
			`SELECT hash FROM audit_chain_events WHERE operation_id = ? AND seq = ?`,
			ev.OperationID, row.Seq-1,
		).Scan(&lastHash)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return StoredEvent{}, fmt.Errorf("audit chain: missing seq %d in %s", row.Seq-1, ev.OperationID)
			}
			return StoredEvent{}, fmt.Errorf("audit chain: read prev hash: %w", err)
		}
		row.PrevHash = lastHash
	}
	row.Hash = eventHash(row, ev.OperationID, snapshot)

	_, err = tx.ExecContext(ctx,
		`INSERT INTO audit_chain_events
			(operation_id, seq, kind, actor, summary, payload, snapshot, auth_method,
			 occurred_at_ns, prev_hash, hash, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.OperationID, row.Seq, row.Kind, row.Actor, row.Summary, row.PayloadJSON, snapshot,
		row.AuthMethod,
		row.OccurredAt.UnixNano(), row.PrevHash, row.Hash, now.Format(time.RFC3339Nano))
	if err != nil {
		if isBusyError(err) {
			return StoredEvent{}, ErrBusy
		}
		return StoredEvent{}, fmt.Errorf("audit chain: insert: %w", err)
	}
	if err := tx.Commit(); err != nil {
		if isBusyError(err) {
			return StoredEvent{}, ErrBusy
		}
		return StoredEvent{}, fmt.Errorf("audit chain: commit: %w", err)
	}
	return row, nil
}

// Read loads one operation's events in seq order, re-verifies the chain
// over the full sequence and returns the timeline together with the
// integrity verdict. The optional from/to window and limit bound which
// events are materialized for display; integrity is always computed over
// the full sequence, so a tampered middle record is reported even when the
// display window does not cover it.
func (s *Store) Read(ctx context.Context, operationID string, from, to time.Time, limit int) (OperationChain, error) {
	operationID = strings.TrimSpace(operationID)
	if err := validateOperationID(operationID); err != nil {
		return OperationChain{}, err
	}
	if limit > maxReadLimit {
		limit = maxReadLimit
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq, kind, actor, summary, payload, auth_method,
		        occurred_at_ns, prev_hash, hash,
		        snapshot,
		        (snapshot IS NOT NULL)
		 FROM audit_chain_events
		 WHERE operation_id = ?
		 ORDER BY seq`,
		operationID,
	)
	if err != nil {
		return OperationChain{}, fmt.Errorf("audit chain: read: %w", err)
	}
	defer rows.Close()

	chain := OperationChain{OperationID: operationID}
	seenAny := false
	// eventsFull holds every record so integrity verifies over the whole
	// sequence; events is the (windowed) display view returned to the
	// caller.
	var eventsFull []StoredEvent
	for rows.Next() {
		var ev StoredEvent
		var ns int64
		var hasSnap bool
		var snap []byte
		if err := rows.Scan(&ev.Seq, &ev.Kind, &ev.Actor, &ev.Summary, &ev.PayloadJSON,
			&ev.AuthMethod, &ns, &ev.PrevHash, &ev.Hash, &snap, &hasSnap); err != nil {
			return OperationChain{}, fmt.Errorf("audit chain: scan: %w", err)
		}
		ev.OccurredAt = time.Unix(0, ns).UTC()
		ev.SnapshotRecorded = hasSnap
		ev.snapshot = snap
		eventsFull = append(eventsFull, ev)
		seenAny = true
	}
	if err := rows.Err(); err != nil {
		return OperationChain{}, fmt.Errorf("audit chain: read: %w", err)
	}
	if !seenAny {
		return OperationChain{}, ErrOperationNotFound
	}

	chain.Integrity = verifyChain(operationID, eventsFull)

	// Build the display view: the from/to window, then the limit.
	display := eventsFull
	if !from.IsZero() || !to.IsZero() {
		filtered := make([]StoredEvent, 0, len(display))
		for _, ev := range display {
			if !from.IsZero() && ev.OccurredAt.Before(from) {
				continue
			}
			if !to.IsZero() && ev.OccurredAt.After(to) {
				continue
			}
			filtered = append(filtered, ev)
		}
		display = filtered
	}
	if limit > 0 && len(display) > limit {
		display = display[:limit]
	}
	chain.Events = display
	return chain, nil
}

// verifyChain recomputes every record hash over the full sequence, checking
// both the prev_hash link and per-record digest, and that seq is
// contiguous.
func verifyChain(operationID string, events []StoredEvent) Integrity {
	prev := genesisHash(operationID)
	prevSeq := int64(0)
	integrity := Integrity{HeadHash: genesisHash(operationID)}
	for _, ev := range events {
		if ev.Seq != prevSeq+1 {
			integrity.FirstBadSeq = ev.Seq
			break
		}
		if ev.PrevHash != prev {
			integrity.FirstBadSeq = ev.Seq
			break
		}
		want := eventHash(ev, operationID, ev.snapshot)
		if want != ev.Hash {
			integrity.FirstBadSeq = ev.Seq
			break
		}
		prevSeq = ev.Seq
		prev = ev.Hash
		integrity.HeadHash = ev.Hash
	}
	if integrity.FirstBadSeq != 0 {
		integrity.Tampered = true
	} else {
		integrity.Intact = true
	}
	return integrity
}

// genesisHash seeds an operation's chain. It is deterministic per
// operation, so verification works without storing a genesis row.
func genesisHash(operationID string) string {
	return sha256Hex(schemaVersion + "|" + operationID)
}

// hashInput is the canonical, unambiguous encoding of every field an
// attacker could rewrite. Marshaling a fixed-struct to JSON (not
// free-text delimiters) means no field can be shifted into another, and
// seq is bound so a single record cannot be relabelled.
type hashInput struct {
	V           int    `json:"v"`
	OperationID string `json:"op"`
	Seq         int64  `json:"seq"`
	Kind        string `json:"kind"`
	Actor       string `json:"actor"`
	AuthMethod  string `json:"auth"`
	Summary     string `json:"summary"`
	OccurredNS  int64  `json:"at_ns"`
	Payload     string `json:"payload"`
	SnapHash    string `json:"snap,omitempty"`
	PrevHash    string `json:"prev"`
}

// eventHash is the record digest. It binds the link to the previous record,
// the sequence number, the identity fields, the human-readable summary, the
// structured payload, the event time and the (encrypted) snapshot.
func eventHash(ev StoredEvent, operationID string, snapshot []byte) string {
	in := hashInput{
		V:           hashVersion,
		OperationID: operationID,
		Seq:         ev.Seq,
		Kind:        ev.Kind,
		Actor:       ev.Actor,
		AuthMethod:  ev.AuthMethod,
		Summary:     ev.Summary,
		OccurredNS:  ev.OccurredAt.UnixNano(),
		Payload:     ev.PayloadJSON,
		PrevHash:    ev.PrevHash,
	}
	if snapshot != nil {
		in.SnapHash = sha256Hex(string(snapshot))
	}
	enc, err := json.Marshal(in)
	if err != nil {
		// json.Marshal of this fixed struct cannot fail; the hash must not
		// silently depend on a failed marshal, so fall back to a
		// deterministic encoding of the same fields.
		return sha256Hex(fallbackHashString(in))
	}
	return sha256Hex(string(enc))
}

// fallbackHashString deterministically encodes the hash input if JSON
// marshaling ever fails (it should not).
func fallbackHashString(in hashInput) string {
	var b strings.Builder
	b.WriteString(strconv.Itoa(in.V))
	b.WriteByte('/')
	b.WriteString(in.OperationID)
	b.WriteByte('/')
	b.WriteString(strconv.FormatInt(in.Seq, 10))
	b.WriteByte('/')
	b.WriteString(in.Kind)
	b.WriteByte('/')
	b.WriteString(in.Actor)
	b.WriteByte('/')
	b.WriteString(in.AuthMethod)
	b.WriteByte('/')
	b.WriteString(in.Summary)
	b.WriteByte('/')
	b.WriteString(strconv.FormatInt(in.OccurredNS, 10))
	b.WriteByte('/')
	b.WriteString(in.Payload)
	b.WriteByte('/')
	b.WriteString(in.SnapHash)
	b.WriteByte('/')
	b.WriteString(in.PrevHash)
	return b.String()
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// isBusyError reports whether a driver error is SQLite busy/locked, i.e.
// the write could not acquire the lock within the busy timeout and is safe
// to retry.
func isBusyError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToUpper(err.Error())
	return strings.Contains(msg, "SQLITE_BUSY") ||
		strings.Contains(msg, "SQLITE_LOCKED") ||
		strings.Contains(msg, "DATABASE IS LOCKED") ||
		strings.Contains(msg, "DATABASE IS BUSY")
}

func validateEvent(ev *Event) error {
	if err := validateOperationID(ev.OperationID); err != nil {
		return err
	}
	if strings.TrimSpace(ev.Kind) == "" {
		return errors.New("audit chain: kind is required")
	}
	if len(ev.Kind) > maxKindLen {
		return fmt.Errorf("audit chain: kind exceeds %d bytes", maxKindLen)
	}
	if ev.Kind == KindApproval && strings.TrimSpace(ev.Actor) == "" {
		return errors.New("audit chain: approval events require an actor")
	}
	if len(ev.Actor) > maxActorLen {
		return fmt.Errorf("audit chain: actor exceeds %d bytes", maxActorLen)
	}
	if len(ev.Summary) > maxSummaryLen {
		return fmt.Errorf("audit chain: summary exceeds %d bytes", maxSummaryLen)
	}
	if len(ev.PayloadJSON) > maxPayloadBytes {
		return fmt.Errorf("audit chain: payload exceeds %d bytes", maxPayloadBytes)
	}
	if ev.PayloadJSON != "" {
		if !json.Valid([]byte(ev.PayloadJSON)) {
			return errors.New("audit chain: payload must be valid JSON")
		}
	}
	if len(ev.ModelInput) > maxSnapshotBytes {
		return fmt.Errorf("audit chain: model_input exceeds %d bytes", maxSnapshotBytes)
	}
	if len(ev.ModelOutput) > maxSnapshotBytes {
		return fmt.Errorf("audit chain: model_output exceeds %d bytes", maxSnapshotBytes)
	}
	// Bound the event time so UnixNano stays defined and stored/read agree.
	if ev.OccurredAt.Before(minEventTime) || ev.OccurredAt.After(maxEventTime) {
		return fmt.Errorf("audit chain: occurred_at out of range %s..%s",
			minEventTime.Format("2006"), maxEventTime.Format("2006"))
	}
	return nil
}

func validateOperationID(operationID string) error {
	id := strings.TrimSpace(operationID)
	if id == "" {
		return fmt.Errorf("%w: required", ErrInvalidOperationID)
	}
	if len(id) > maxOperationIDLen {
		return fmt.Errorf("%w: exceeds %d bytes", ErrInvalidOperationID, maxOperationIDLen)
	}
	if !isOperationIDChars(id) {
		return fmt.Errorf("%w: must match [A-Za-z0-9._:-]", ErrInvalidOperationID)
	}
	return nil
}

func isOperationIDChars(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == ':' || r == '-':
		default:
			return false
		}
	}
	return true
}
