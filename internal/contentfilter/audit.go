package contentfilter

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
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
	// HostDBPath 已废弃：RIC-442 根治后 CPA 不再以宿主 UID 直接打开
	// KEEPER 的 SQLite 文件，保留字段仅为兼容旧 env 与测试。
	//
	// Deprecated: 直接以 mode=rwc 写 KEEPER 的 app.db 会让属主漂移到宿主
	// UID，KEEPER 随后 "attempt to write a readonly database" 无法登录。
	HostDBPath string
	// ContainerName / ContainerDBPath are deprecated docker-cp 通道字段；
	// RIC-442 后 CPA 不再 cp-in 到 KEEPER 容器，仍保留以兼容旧 env。
	//
	// Deprecated: 同 HostDBPath，写入会把 app.db 重建为宿主 UID。
	ContainerName   string
	ContainerDBPath string
	// DockerCmd is the docker CLI binary (defaults to "docker"). Deprecated
	// alongside the docker-cp 通道。
	//
	// Deprecated: see HostDBPath.
	DockerCmd string
	// KEEPERAuditURL 是 KEEPER 暴露的审计 ingest 端点 base URL，例如
	// http://127.0.0.1:4320。ServerOption 默认拼上
	// /api/v1/contentfilter/logs/ingest。KEEPER 挂该路由的中间件
	// adminOrManagementKeyMiddleware 同时接受 admin 会话与
	// X-CPA-Management-Key 共享密钥；本通道使用密钥。
	KEEPERAuditURL string
	// KEEPERManagementKey 是 CPA↔KEEPER 共享管理密钥（与 KEEPER 配置的
	// CPA_MANAGEMENT_KEY 一致），通过 X-CPA-Management-Key 头传递。
	KEEPERManagementKey string
	// HTTPClient 自定义注入，便于测试拦截；nil 时使用 withDefaults 内的
	// 默认 client（WriteTimeout 控制单次请求超时）。
	HTTPClient *http.Client
	// SidecarPath, when set, overrides the default local sidecar location
	// used as a write buffer when the HTTP channel is unavailable. Audit
	// rows are written to the sidecar immediately and a flush hook can
	// later replay them when the channel recovers.
	SidecarPath string
	// QueueSize caps the in-memory audit queue depth. Producers are
	// non-blocking: an enqueue that would overflow drops the row and bumps
	// a counter (best-effort: a missed audit row must never block a request).
	QueueSize int
	// WriteTimeout bounds each individual write to KEEPER.
	WriteTimeout time.Duration
	// BatchSize caps the number of rows flushed in a single HTTP 请求。
	// 0 (the default) 保留单行行为；生产典型调优 64-256。
	BatchSize int
	// BatchInterval caps the maximum time a row can sit in a pending
	// batch before the worker flushes it. Default 200ms; set to 0 to
	// disable the timer (rows flush on queue full or on graceful close).
	BatchInterval time.Duration
}

func (e *AuditEnv) withDefaults() {
	// RIC-442: 不再为 KEEPER volume 路径、容器名、docker 命令设默认；
	// 这些字段仅在调用方显式传入时（兼容旧 env 或测试）使用。新部署应留空
	// 让审计走 HTTP 通道。
	if e.SidecarPath == "" {
		e.SidecarPath = defaultSidecarPath()
	}
	if e.QueueSize <= 0 {
		e.QueueSize = 1024
	}
	if e.WriteTimeout <= 0 {
		e.WriteTimeout = 10 * time.Second
	}
	if e.BatchSize <= 0 {
		e.BatchSize = 1
	}
	if e.BatchInterval < 0 {
		e.BatchInterval = 0
	}
	if e.BatchInterval == 0 && e.BatchSize > 1 {
		// 200ms is a sensible production timer; tests can override.
		e.BatchInterval = 200 * time.Millisecond
	}
	if e.HTTPClient == nil {
		e.HTTPClient = &http.Client{Timeout: e.WriteTimeout}
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
	Matches         string // JSON array (RIC-440) — was CSV in RIC-438
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

// MarshalMatchesJSON serialises a slice of matched values as a JSON array.
// It falls back to a string with values joined by "," when JSON encoding
// fails (which only happens for non-UTF-8 inputs — a corrupt capture value).
// Storing a JSON array lets the KEEPER audit UI render each match as a
// chip without a CSV parser.
func MarshalMatchesJSON(values []string) string {
	if len(values) == 0 {
		return "[]"
	}
	b, err := json.Marshal(values)
	if err != nil {
		return strings.Join(values, ",")
	}
	return string(b)
}

// Audit is the async writer for content_filter_logs. It is safe for
// concurrent use by the middleware; producers call Enqueue (non-blocking)
// and a single background worker drains the queue and writes to KEEPER.
//
// The writer does not touch the content_filter_rules table. Audit write
// failures are logged and never propagated to the request hot path.
//
// RIC-440 (2026-08-31) adds batched INSERTs: when env.BatchSize > 1 the
// worker accumulates rows in memory and flushes them in a single
// transaction. The trade-off: a few rows of latency for up to ~N x lower
// per-row SQLite write cost, which dominates total throughput once the
// queue depth is non-trivial.
type Audit struct {
	env   AuditEnv
	queue chan AuditRow

	stop    chan struct{}
	once    sync.Once
	stopped atomic.Bool

	// counters for observability.
	enqueued atomic.Uint64
	dropped  atomic.Uint64
	written  atomic.Uint64
	failed   atomic.Uint64
	batches  atomic.Uint64
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
	logger.WithFields(map[string]any{
		"queue":          env.QueueSize,
		"batch_size":     env.BatchSize,
		"batch_interval": env.BatchInterval,
	}).Info("content filter audit writer started")
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
func (a *Audit) Stats() (enqueued, dropped, written, failed, batches uint64) {
	if a == nil {
		return
	}
	return a.enqueued.Load(), a.dropped.Load(), a.written.Load(), a.failed.Load(), a.batches.Load()
}

// Close drains the queue and stops the worker. The current batch (if any) is
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
		"batches":  a.batches.Load(),
	}).Info("content filter audit writer stopped")
}

// loop is the single-worker drain. When BatchSize > 1 the loop accumulates
// rows in a slice until the slice reaches BatchSize, BatchInterval elapses
// since the first row, or the queue is closed.
func (a *Audit) loop() {
	defer close(a.stop)
	batchSize := a.env.BatchSize
	batchInterval := a.env.BatchInterval
	batch := make([]AuditRow, 0, batchSize)
	var deadline <-chan time.Time
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := a.writeRows(batch); err != nil {
			a.failed.Add(uint64(len(batch)))
			logger.WithError(err).Warn("content filter audit batch write failed")
		} else {
			a.written.Add(uint64(len(batch)))
			a.batches.Add(1)
		}
		batch = batch[:0]
		deadline = nil
	}
	for {
		select {
		case row, ok := <-a.queue:
			if !ok {
				flush()
				return
			}
			batch = append(batch, row)
			if batchSize <= 1 || len(batch) >= batchSize {
				flush()
				continue
			}
			if batchInterval > 0 && deadline == nil {
				deadline = time.After(batchInterval)
			}
		case <-deadline:
			flush()
		}
	}
}

// writeRows persists a batch of rows. The first successful channel (HTTP,
// legacy direct, sidecar) is used for the whole batch so a single logical
// commit either lands all rows or none.
//
// RIC-442 架构: KEEPER 是 app.db 的唯一写者；HTTP 通道是首选（POST
// /api/v1/contentfilter/logs/ingest，X-CPA-Management-Key 头鉴权），
// 由 KEEPER 自己的进程完成 INSERT。原 writeBatchDirect (mode=rwc) 与
// writeBatchViaDockerCP 通道被移除——它们以宿主 UID 写入 KEEPER 的卷会
// 把属主漂移到 501，KEEPER 随后 "attempt to write a readonly database"，
// 登录与统计全部失败。
func (a *Audit) writeRows(rows []AuditRow) error {
	if len(rows) == 0 {
		return nil
	}
	if a.env.KEEPERAuditURL != "" && a.env.KEEPERManagementKey != "" {
		if err := a.writeBatchHTTP(rows); err == nil {
			return nil
		} else {
			logger.WithError(err).Debug("audit http write failed, falling back to sidecar")
		}
	}
	// 兼容旧配置/测试：若显式提供了 HostDBPath / ContainerName，保留通道。
	// 新部署应留空这两字段。
	if a.env.HostDBPath != "" {
		if err := a.writeBatchDirectLegacy(rows); err == nil {
			return nil
		} else {
			logger.WithError(err).Debug("audit direct write failed, falling back to sidecar")
		}
	}
	if a.env.ContainerName != "" {
		if err := a.writeBatchViaDockerCPLegacy(rows); err == nil {
			return nil
		} else {
			logger.WithError(err).Debug("audit docker-cp write failed, falling back to sidecar")
		}
	}
	return a.writeBatchSidecar(rows)
}

// writeRow remains for backward compatibility with single-row callers; the
// batched path is preferred (writeRows) and reaches the same channels.
func (a *Audit) writeRow(row AuditRow) error {
	return a.writeRows([]AuditRow{row})
}

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

// openWritableKEEPER opens a local SQLite file in write mode with a short
// busy timeout. 保留供 legacy 通道（writeBatchDirectLegacy /
// writeBatchViaDockerCPLegacy）使用；新部署不应再触发这些路径。
func openWritableKEEPER(path string, timeout time.Duration) (*sql.DB, error) {
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

// writeBatchDirectLegacy keeps the pre-RIC-442 direct write path for tests
// and explicitly-configured legacy envs. New deployments must not enable it;
// see AuditEnv.HostDBPath deprecation note.
func (a *Audit) writeBatchDirectLegacy(rows []AuditRow) error {
	db, err := openWritableKEEPER(a.env.HostDBPath, a.env.WriteTimeout)
	if err != nil {
		return err
	}
	defer db.Close()
	return insertLogBatch(db, rows)
}

// writeBatchViaDockerCPLegacy keeps the pre-RIC-442 docker-cp write-back path
// for tests and explicitly-configured legacy envs. New deployments must not
// enable it; see AuditEnv.ContainerName deprecation note.
func (a *Audit) writeBatchViaDockerCPLegacy(rows []AuditRow) error {
	tmp, err := os.CreateTemp("", "keeper-audit-*.db")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	_ = tmp.Close()
	_ = os.Remove(tmpName)

	ctx, cancel := context.WithTimeout(context.Background(), a.env.WriteTimeout)
	defer cancel()

	dc := a.env.DockerCmd
	if dc == "" {
		dc = "docker"
	}
	cmd := exec.CommandContext(ctx, dc, "cp",
		fmt.Sprintf("%s:%s", a.env.ContainerName, a.env.ContainerDBPath), tmpName)
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("docker cp out: %v: %s", err, strings.TrimSpace(string(out)))
	}

	db, err := openWritableKEEPER(tmpName, a.env.WriteTimeout)
	if err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("open temp: %w", err)
	}
	writeErr := insertLogBatch(db, rows)
	_ = db.Close()
	if writeErr != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("insert: %w", writeErr)
	}

	cmd = exec.CommandContext(ctx, dc, "cp",
		tmpName, fmt.Sprintf("%s:%s", a.env.ContainerName, a.env.ContainerDBPath))
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("docker cp in: %v: %s", err, strings.TrimSpace(string(out)))
	}
	_ = os.Remove(tmpName)
	return nil
}

// writeBatchHTTP POSTs a batch of rows to KEEPER's audit ingest endpoint.
// KEEPER 进程以 app:app 写入自己的 app.db——CPA 进程不再以宿主 UID 打开
// KEEPER 的 SQLite 文件，从根上消除 RIC-442 描述的属主漂移与 readonly
// 复发。
func (a *Audit) writeBatchHTTP(rows []AuditRow) error {
	body := auditHTTPRequest{Logs: make([]auditHTTPRow, 0, len(rows))}
	for _, r := range rows {
		body.Logs = append(body.Logs, auditRowToHTTP(r))
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal audit batch: %w", err)
	}
	base := strings.TrimRight(a.env.KEEPERAuditURL, "/")
	url := base + "/api/v1/contentfilter/logs/ingest"
	ctx, cancel := context.WithTimeout(context.Background(), a.env.WriteTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("build audit request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CPA-Management-Key", a.env.KEEPERManagementKey)
	req.Header.Set("X-CPA-Usage-Keeper-Request", "fetch") // 与 KEEPER request-intent 一致
	client := a.env.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: a.env.WriteTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("audit http post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("audit http status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

// auditHTTPRequest 是 KEEPER audit ingest 端点的请求体；与 KEEPER 侧
// contentFilterLogIngestRequest 完全对齐（RIC-442）。
type auditHTTPRequest struct {
	Logs []auditHTTPRow `json:"logs"`
}

type auditHTTPRow struct {
	RuleID          int64  `json:"rule_id"`
	RuleName        string `json:"rule_name"`
	FilterType      string `json:"filter_type"`
	MatchCount      int    `json:"match_count"`
	Matches         string `json:"matches"`
	Action          string `json:"action"`
	Model           string `json:"model"`
	ClientIP        string `json:"client_ip"`
	UserID          string `json:"user_id"`
	RawPreview      string `json:"raw_preview"`
	FilteredPreview string `json:"filtered_preview"`
	CreatedAt       string `json:"created_at"`
}

func auditRowToHTTP(r AuditRow) auditHTTPRow {
	ts := r.CreatedAt
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	return auditHTTPRow{
		RuleID:          r.RuleID,
		RuleName:        r.RuleName,
		FilterType:      r.FilterType,
		MatchCount:      r.MatchCount,
		Matches:         r.Matches,
		Action:          r.Action,
		Model:           r.Model,
		ClientIP:        r.ClientIP,
		UserID:          r.UserID,
		RawPreview:      r.RawPreview,
		FilteredPreview: r.FilteredPreview,
		CreatedAt:       ts.UTC().Format(time.RFC3339),
	}
}

// writeBatchSidecar writes the rows to a local sidecar SQLite file with the
// same content_filter_logs schema. This is the final fallback: the rows are
// never lost, even if the KEEPER write channel is unavailable.
func (a *Audit) writeBatchSidecar(rows []AuditRow) error {
	if err := os.MkdirAll(filepath.Dir(a.env.SidecarPath), 0o755); err != nil {
		return err
	}
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
	if err := insertLogBatch(db, rows); err != nil {
		return err
	}
	if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return err
	}
	return nil
}

// writeSidecar remains for single-row callers; wraps writeBatchSidecar.
func (a *Audit) writeSidecar(row AuditRow) error {
	return a.writeBatchSidecar([]AuditRow{row})
}

// ensureSidecarSchema creates the content_filter_logs table in the local
// sidecar if it does not already exist. The schema mirrors the KEEPER
// table exactly so the sidecar can be merged later if needed.
//
// RIC-440 (2026-08-31) adds indexes on the hot query columns (created_at,
// rule_id, model, user_id, client_ip) so the KEEPER audit page can paginate
// and filter without scanning the whole table. The KEEPER sidecar path is
// local-only; the KEEPER production schema migration is a separate
// concern handled by the KEEPER team.
func ensureSidecarSchema(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS content_filter_logs (
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
	)`); err != nil {
		return err
	}
	indexes := []string{
		`CREATE INDEX IF NOT EXISTS idx_cflogs_created_at ON content_filter_logs(created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_cflogs_rule_id ON content_filter_logs(rule_id)`,
		`CREATE INDEX IF NOT EXISTS idx_cflogs_model ON content_filter_logs(model)`,
		`CREATE INDEX IF NOT EXISTS idx_cflogs_user_id ON content_filter_logs(user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_cflogs_client_ip ON content_filter_logs(client_ip)`,
		`CREATE INDEX IF NOT EXISTS idx_cflogs_rule_created ON content_filter_logs(rule_id, created_at)`,
	}
	for _, ddl := range indexes {
		if _, err := db.Exec(ddl); err != nil {
			logger.WithError(err).Debug("audit index create failed (non-fatal)")
		}
	}
	return nil
}

// insertLogBatch writes a batch of rows in a single transaction. The order of
// rows is preserved within the transaction; modernc.org/sqlite guarantees
// sequential AUTOINCREMENT ids so a downstream consumer reading ORDER BY id
// sees them in enqueue order.
func insertLogBatch(db *sql.DB, rows []AuditRow) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(
		`INSERT INTO content_filter_logs
			(rule_id, rule_name, filter_type, match_count, matches, action, model, client_ip, user_id, raw_preview, filtered_preview, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer stmt.Close()
	for i := range rows {
		r := rows[i]
		if r.Action == "" {
			r.Action = "mask"
		}
		if r.FilterType == "" {
			r.FilterType = "mask"
		}
		if _, err := stmt.Exec(
			nullableInt(r.RuleID),
			nullString(r.RuleName),
			r.FilterType,
			r.MatchCount,
			r.Matches,
			r.Action,
			nullString(r.Model),
			nullString(r.ClientIP),
			nullString(r.UserID),
			nullString(r.RawPreview),
			nullString(r.FilteredPreview),
			r.CreatedAt.UTC().Format("2006-01-02 15:04:05"),
		); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// insertLogRow writes one row. Kept for compatibility with single-row callers;
// production code should use insertLogBatch.
func insertLogRow(db *sql.DB, row AuditRow) error {
	return insertLogBatch(db, []AuditRow{row})
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
//
// RIC-440 (2026-08-31) tightens the default cap from 4 KiB to 1 KiB. The
// audit log had grown past 6 GiB on production KEEPER instances; previews
// dominated row size. The cap is still large enough to capture the
// matching context for a triage (a 1 KiB snippet holds the matched line
// plus ~200 chars on each side), and an operator can request the full
// body via the request-id lookup in the gateway trace.
func truncatePreview(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}
