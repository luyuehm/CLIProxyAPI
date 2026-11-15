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
	"strings"
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

	maxOperationIDLen = 128
	maxKindLen        = 32
	maxActorLen       = 256
	maxSummaryLen     = 4096
	// maxPayloadBytes bounds a single JSON payload; the evidence contract
	// is summary + structured args, not full request bodies.
	maxPayloadBytes = 64 << 10
	// maxSnapshotBytes bounds each of model_input / model_output.
	maxSnapshotBytes = 256 << 10
)

var (
	// ErrOperationNotFound is returned by Append when the operation has
	// never been started; callers create it implicitly by appending the
	// first event.
	ErrOperationNotFound = errors.New("audit chain: operation not found")
	// ErrInvalidKey is returned for a malformed encryption key.
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
	// FirstBadSeq is the seq of the first record whose hash does not match
	// the recomputation. Zero when intact.
	FirstBadSeq int64
	// HeadHash is the hash of the last verified record: the digest of the
	// intact prefix.
	HeadHash string
	// BackupDigest is the HeadHash restated as the "original" digest of
	// what is still provable, present only when the chain is broken.
	BackupDigest string `json:"backup_digest,omitempty"`
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
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
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
		occurred_at TEXT NOT NULL,
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
// atomically. The whole append is one transaction so seq allocation and
// row insertion can never diverge.
func (s *Store) Append(ctx context.Context, ev Event, key []byte) (StoredEvent, error) {
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
		obj := map[string]string{"model_input": ev.ModelInput, "model_output": ev.ModelOutput}
		plain, err := json.Marshal(obj)
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

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return StoredEvent{}, fmt.Errorf("audit chain: begin tx: %w", err)
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			// Swallow: the original error is returned by the caller.
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
			 occurred_at, occurred_at_ns, prev_hash, hash, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.OperationID, row.Seq, row.Kind, row.Actor, row.Summary, row.PayloadJSON, snapshot,
		row.AuthMethod,
		row.OccurredAt.Format(time.RFC3339Nano), row.OccurredAt.UnixNano(),
		row.PrevHash, row.Hash, now.Format(time.RFC3339Nano))
	if err != nil {
		return StoredEvent{}, fmt.Errorf("audit chain: insert: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return StoredEvent{}, fmt.Errorf("audit chain: commit: %w", err)
	}
	return row, nil
}

// Read loads one operation's events in seq order, re-verifies the chain
// and returns the timeline together with the integrity verdict. The
// optional from/to window filters the returned events; integrity is always
// verified over the full sequence, so a tampered middle record is reported
// even when the read window does not cover it.
func (s *Store) Read(ctx context.Context, operationID string, from, to time.Time) (OperationChain, error) {
	if err := validateOperationID(operationID); err != nil {
		return OperationChain{}, err
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
		chain.Events = append(chain.Events, ev)
		seenAny = true
	}
	if err := rows.Err(); err != nil {
		return OperationChain{}, fmt.Errorf("audit chain: read: %w", err)
	}
	if !seenAny {
		return OperationChain{}, ErrOperationNotFound
	}

	chain.Integrity = verifyChain(operationID, chain.Events)

	// Apply the time window to the display view only.
	if !from.IsZero() || !to.IsZero() {
		filtered := make([]StoredEvent, 0, len(chain.Events))
		for _, ev := range chain.Events {
			if !from.IsZero() && ev.OccurredAt.Before(from) {
				continue
			}
			if !to.IsZero() && ev.OccurredAt.After(to) {
				continue
			}
			filtered = append(filtered, ev)
		}
		chain.Events = filtered
	}
	return chain, nil
}

// verifyChain recomputes every record hash over the full sequence.
func verifyChain(operationID string, events []StoredEvent) Integrity {
	prev := genesisHash(operationID)
	verified := 0
	integrity := Integrity{HeadHash: genesisHash(operationID)}
	for _, ev := range events {
		if ev.PrevHash != prev {
			integrity.FirstBadSeq = ev.Seq
			break
		}
		want := eventHash(ev, operationID, ev.snapshot)
		if want != ev.Hash {
			integrity.FirstBadSeq = ev.Seq
			break
		}
		prev = ev.Hash
		integrity.HeadHash = ev.Hash
		verified++
	}
	if integrity.FirstBadSeq != 0 {
		integrity.Tampered = true
		integrity.BackupDigest = integrity.HeadHash
	} else {
		integrity.Intact = true
	}
	_ = verified
	return integrity
}

// genesisHash seeds an operation's chain. It is deterministic per
// operation, so verification works without storing a genesis row.
func genesisHash(operationID string) string {
	h := sha256Hex(schemaVersion + "|" + operationID)
	return h
}

// eventHash is the record digest. It binds every field an attacker could
// rewrite: the link to the previous record, the identity fields, the
// human-readable summary, the structured payload, the event time and the
// (encrypted) snapshot.
func eventHash(ev StoredEvent, operationID string, snapshot []byte) string {
	var b strings.Builder
	b.WriteString(ev.PrevHash)
	b.WriteByte('|')
	b.WriteString(operationID)
	b.WriteByte('|')
	b.WriteString(ev.Kind)
	b.WriteByte('|')
	b.WriteString(ev.Actor)
	b.WriteByte('|')
	b.WriteString(ev.AuthMethod)
	b.WriteByte('|')
	b.WriteString(ev.Summary)
	b.WriteByte('|')
	fmt.Fprintf(&b, "%d", ev.OccurredAt.UnixNano())
	b.WriteByte('|')
	b.WriteString(sha256Hex(ev.PayloadJSON))
	if snapshot != nil {
		b.WriteString(sha256Hex(string(snapshot)))
	}
	return sha256Hex(b.String())
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
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
	return nil
}

func validateOperationID(operationID string) error {
	id := strings.TrimSpace(operationID)
	if id == "" {
		return errors.New("audit chain: operation id is required")
	}
	if len(id) > maxOperationIDLen {
		return fmt.Errorf("audit chain: operation id exceeds %d bytes", maxOperationIDLen)
	}
	if !isOperationIDChars(id) {
		return errors.New("audit chain: operation id must match [A-Za-z0-9._:-]")
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
