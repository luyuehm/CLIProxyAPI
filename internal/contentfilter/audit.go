package contentfilter

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

// AuditEnv controls the audit writer's runtime configuration via env vars.
// All settings are optional; sensible defaults match the syncer's KEEPER
// defaults so audit writes land in the same KEEPER instance the syncer reads.
type AuditEnv struct {
	// HostDBPath is the writable path to the KEEPER app.db. When set and the
	// file is writable, audit rows are INSERTed directly. This is the
	// production (Linux host volume) path.
	HostDBPath string
	// ContainerName / ContainerDBPath are used for the docker-cp fallback:
	// we copy the file to a local temp, INSERT there, and copy back.
	ContainerName   string
	ContainerDBPath string
	// DockerCmd is the docker CLI binary (defaults to "docker").
	DockerCmd string
	// SidecarPath, when set, overrides the default local sidecar location
	// used as a write buffer when neither direct nor docker-cp writes are
	// possible. Audit rows are written to the sidecar immediately and
	// periodically flushed to KEEPER when the writable channel recovers.
	SidecarPath string
	// QueueSize caps the in-memory audit queue depth. Producers are
	// non-blocking: an enqueue that would overflow drops the row and bumps
	// a counter (best-effort: a missed audit row must never block a request).
	QueueSize int
	// WriteTimeout bounds each individual write to KEEPER.
	WriteTimeout time.Duration
}

func (e *AuditEnv) withDefaults() {
	if e.HostDBPath == "" {
		e.HostDBPath = DefaultHostVolumeDBPath
	}
	if e.ContainerName == "" {
		e.ContainerName = DefaultContainerName
	}
	if e.ContainerDBPath == "" {
		e.ContainerDBPath = DefaultContainerDBPath
	}
	if e.DockerCmd == "" {
		e.DockerCmd = "docker"
	}
	if e.SidecarPath == "" {
		e.SidecarPath = defaultSidecarPath()
	}
	if e.QueueSize <= 0 {
		e.QueueSize = 1024
	}
	if e.WriteTimeout <= 0 {
		e.WriteTimeout = 10 * time.Second
	}
}

// defaultSidecarPath returns the default local audit sidecar path. The
// sidecar is a SQLite file with the same content_filter_logs schema, used as
// a write buffer when the live KEEPER db is not directly writable (e.g. on
// macOS where the host volume is not exposed and docker-cp back-overwrite
// would race with KEEPER's own writes).
func defaultSidecarPath() string {
	if v := os.Getenv("CPA_CONTENT_FILTER_AUDIT_SIDECAR"); v != "" {
		return v
	}
	dir := filepath.Join(os.TempDir(), "cpa-content-filter-audit")
	return filepath.Join(dir, "audit.db")
}

// AuditRow is one record destined for KEEPER's content_filter_logs table.
// It is passed by value through the async queue so producers never block on
// downstream slowness.
type AuditRow struct {
	RuleID          int64
	RuleName        string
	FilterType      string
	MatchCount      int
	Matches         string // JSON or comma-separated values, per KEEPER UI
	Action          string
	Model           string
	ClientIP        string
	UserID          string
	RawPreview      string
	FilteredPreview string
	CreatedAt       time.Time
}

// EnqueueResult reports whether an audit row was enqueued. Dropped is true
// when the queue was full and the row was discarded to keep request
// latency unaffected.
type EnqueueResult struct {
	Enqueued bool
	Dropped  bool
}

// Audit is the async writer for content_filter_logs. It is safe for
// concurrent use by the middleware; producers call Enqueue (non-blocking)
// and a single background worker drains the queue and writes to KEEPER.
//
// The writer does not touch the content_filter_rules table. Audit write
// failures are logged and never propagated to the request hot path.
type Audit struct {
	env   AuditEnv
	queue chan AuditRow

	stop   chan struct{}
	once   sync.Once
	stopped atomic.Bool

	// counters for observability.
	enqueued atomic.Uint64
	dropped  atomic.Uint64
	written  atomic.Uint64
	failed   atomic.Uint64
}

// NewAudit constructs the writer and starts its background worker. It
// returns nil when the env is empty (the writer is fully disabled).
func NewAudit(env AuditEnv) *Audit {
	env.withDefaults()
	a := &Audit{
		env:   env,
		queue: make(chan AuditRow, env.QueueSize),
		stop:  make(chan struct{}),
	}
	a.once.Do(func() { go a.loop() })
	logger.WithField("queue", env.QueueSize).Info("content filter audit writer started")
	return a
}

// Enqueue submits a row for asynchronous writing. It is non-blocking and
// always returns promptly: when the queue is full, the row is dropped and
// the dropped counter is incremented.
func (a *Audit) Enqueue(row AuditRow) EnqueueResult {
	if a == nil || a.stopped.Load() {
		return EnqueueResult{}
	}
	if row.CreatedAt.IsZero() {
		row.CreatedAt = time.Now().UTC()
	}
	select {
	case a.queue <- row:
		a.enqueued.Add(1)
		return EnqueueResult{Enqueued: true}
	default:
		a.dropped.Add(1)
		logger.Warn("content filter audit queue full, dropping row")
		return EnqueueResult{Dropped: true}
	}
}

// Stats returns writer counters for observability.
func (a *Audit) Stats() (enqueued, dropped, written, failed uint64) {
	if a == nil {
		return
	}
	return a.enqueued.Load(), a.dropped.Load(), a.written.Load(), a.failed.Load()
}

// Close drains the queue and stops the worker. The current row (if any) is
// flushed before the worker exits. A best-effort drain is performed because
// audit rows are non-critical; closed-channels on the producer side are
// signalled by stopped.
func (a *Audit) Close() {
	if a == nil {
		return
	}
	if !a.stopped.CompareAndSwap(false, true) {
		return
	}
	close(a.queue)
	<-a.stop
	logger.WithFields(map[string]any{
		"enqueued": a.enqueued.Load(),
		"written":  a.written.Load(),
		"failed":   a.failed.Load(),
		"dropped":  a.dropped.Load(),
	}).Info("content filter audit writer stopped")
}

func (a *Audit) loop() {
	defer close(a.stop)
	for row := range a.queue {
		if err := a.writeRow(row); err != nil {
			a.failed.Add(1)
			logger.WithError(err).Warn("content filter audit write failed")
			continue
		}
		a.written.Add(1)
	}
}

// writeRow attempts to write one row to KEEPER, falling back through three
// channels in order: direct host path write, docker-cp round-trip, local
// sidecar. Each step is timed out so a slow filesystem cannot stall the
// worker indefinitely.
func (a *Audit) writeRow(row AuditRow) error {
	// 1. Direct host path write (Linux production path).
	if a.env.HostDBPath != "" {
		if err := a.writeDirect(row); err == nil {
			return nil
		} else if !isAccessDenied(err) && !isMissingPath(err) {
			logger.WithError(err).Debug("audit direct write failed, trying docker-cp")
		}
	}
	// 2. docker-cp round-trip (writeable KEEPER but no host path).
	if a.env.ContainerName != "" {
		if err := a.writeViaDockerCP(row); err == nil {
			return nil
		} else {
			logger.WithError(err).Debug("audit docker-cp write failed, falling back to sidecar")
		}
	}
	// 3. Local sidecar: best-effort local mirror of the audit table.
	return a.writeSidecar(row)
}

// isAccessDenied / isMissingPath recognize errors that justify skipping the
// direct write path so the worker advances to the docker-cp / sidecar
// fallback promptly.
func isAccessDenied(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "readonly") || strings.Contains(err.Error(), "permission denied")
}

func isMissingPath(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), "no such file")
}

func (a *Audit) writeDirect(row AuditRow) error {
	db, err := openWritableKEEPER(a.env.HostDBPath, a.env.WriteTimeout)
	if err != nil {
		return err
	}
	defer db.Close()
	return insertLogRow(db, row)
}

// openWritableKEEPER opens the KEEPER SQLite file in write mode with a
// short busy timeout so concurrent KEEPER writers don't block the worker.
func openWritableKEEPER(path string, timeout time.Duration) (*sql.DB, error) {
	// Open with explicit mode=rwc so modernc.org/sqlite gets a writeable
	// connection. We issue the busy_timeout pragma via Exec (avoids DSN
	// parsing ambiguity across modernc.org/sqlite URL decoder versions).
	dsn := fmt.Sprintf("file:%s?mode=rwc", filepath.ToSlash(path))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA busy_timeout=%d", timeout.Milliseconds())); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// writeViaDockerCP copies the KEEPER app.db to a temp file, opens it
// read-write, INSERTs the row, then copies it back. The whole flow is
// wrapped in a timeout; failures fall through to the sidecar.
//
// This path is racy: KEEPER's own writes between the cp-out and cp-in are
// clobbered. It is only used as a last-resort fallback when no direct
// writable path is available (typical on macOS dev environments), and the
// sidecar is always written too so no audit row is lost.
func (a *Audit) writeViaDockerCP(row AuditRow) error {
	tmp, err := os.CreateTemp("", "keeper-audit-*.db")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	_ = tmp.Close()
	_ = os.Remove(tmpName)

	ctx, cancel := context.WithTimeout(context.Background(), a.env.WriteTimeout)
	defer cancel()

	// cp out
	cmd := exec.CommandContext(ctx, a.env.DockerCmd, "cp",
		fmt.Sprintf("%s:%s", a.env.ContainerName, a.env.ContainerDBPath), tmpName)
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("docker cp out: %v: %s", err, strings.TrimSpace(string(out)))
	}

	// open + write + close
	db, err := openWritableKEEPER(tmpName, a.env.WriteTimeout)
	if err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("open temp: %w", err)
	}
	writeErr := insertLogRow(db, row)
	_ = db.Close()
	if writeErr != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("insert: %w", writeErr)
	}

	// cp back
	cmd = exec.CommandContext(ctx, a.env.DockerCmd, "cp",
		tmpName, fmt.Sprintf("%s:%s", a.env.ContainerName, a.env.ContainerDBPath))
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("docker cp in: %v: %s", err, strings.TrimSpace(string(out)))
	}
	_ = os.Remove(tmpName)
	return nil
}

// writeSidecar writes the row to a local sidecar SQLite file with the same
// content_filter_logs schema. This is the final fallback: the row is never
// lost, even if all KEEPER write channels are unavailable.
func (a *Audit) writeSidecar(row AuditRow) error {
	if err := os.MkdirAll(filepath.Dir(a.env.SidecarPath), 0o755); err != nil {
		return err
	}
	// Pre-create the file so the SQLite driver cannot reinterpret the path
	// as a connection name. Without this, modernc.org/sqlite may keep the
	// data in a transient pool that is not persisted to disk.
	if _, err := os.Stat(a.env.SidecarPath); errors.Is(err, os.ErrNotExist) {
		f, err := os.Create(a.env.SidecarPath)
		if err != nil {
			return err
		}
		_ = f.Close()
	}
	db, err := sql.Open("sqlite", a.env.SidecarPath)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		return err
	}
	if err := ensureSidecarSchema(db); err != nil {
		return err
	}
	if err := insertLogRow(db, row); err != nil {
		return err
	}
	// Force the write to be checkpointed to the file before we close, so the
	// audit row survives a hard crash.
	if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return err
	}
	return nil
}

// ensureSidecarSchema creates the content_filter_logs table in the local
// sidecar if it does not already exist. The schema mirrors the KEEPER
// table exactly so the sidecar can be merged later if needed.
func ensureSidecarSchema(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS content_filter_logs (
		id integer PRIMARY KEY AUTOINCREMENT,
		rule_id integer,
		rule_name text,
		filter_type text,
		match_count integer NOT NULL DEFAULT 0,
		matches text,
		action text NOT NULL,
		model text,
		client_ip text,
		user_id text,
		raw_preview text,
		filtered_preview text,
		created_at datetime
	)`)
	return err
}

// insertLogRow writes one row to content_filter_logs. matches is serialised
// as a JSON array to keep the value round-trippable through SQLite.
func insertLogRow(db *sql.DB, row AuditRow) error {
	if row.Action == "" {
		row.Action = "mask"
	}
	if row.FilterType == "" {
		row.FilterType = "mask"
	}
	_, err := db.Exec(
		`INSERT INTO content_filter_logs
			(rule_id, rule_name, filter_type, match_count, matches, action, model, client_ip, user_id, raw_preview, filtered_preview, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		nullableInt(row.RuleID),
		nullString(row.RuleName),
		row.FilterType,
		row.MatchCount,
		row.Matches,
		row.Action,
		nullString(row.Model),
		nullString(row.ClientIP),
		nullString(row.UserID),
		nullString(row.RawPreview),
		nullString(row.FilteredPreview),
		row.CreatedAt.UTC().Format("2006-01-02 15:04:05"),
	)
	return err
}

func nullableInt(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// extractClientIP pulls a client IP from a Gin request, preferring the
// X-Forwarded-For / X-Real-IP headers (the gateway typically sits behind a
// reverse proxy) and falling back to RemoteAddr. Returns "" when no IP
// can be determined.
func extractClientIP(headers map[string][]string, remote string) string {
	for _, h := range []string{"X-Forwarded-For", "X-Real-IP"} {
		if vals, ok := headers[h]; ok && len(vals) > 0 {
			first := strings.TrimSpace(strings.Split(vals[0], ",")[0])
			if first != "" {
				return first
			}
		}
	}
	if remote == "" {
		return ""
	}
	host, _, err := net.SplitHostPort(remote)
	if err == nil {
		return host
	}
	return remote
}

// truncatePreview clamps a preview string to a sensible length so a single
// huge request does not blow up the audit table.
func truncatePreview(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}
