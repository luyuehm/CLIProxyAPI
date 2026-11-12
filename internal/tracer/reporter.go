package tracer

import (
	"bytes"
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

// managementKeyHeader is the shared KEEPER↔CPA management key header.
// KEEPER's trace ingest is mounted behind adminOrManagementKeyMiddleware,
// so the key authenticates the reporter machine-to-machine, matching the
// content-filter audit and arbitrage channels.
const managementKeyHeader = "X-CPA-Management-Key"

// ingestTokenHeader is the dedicated trace-ingest token header. KEEPER's
// ingest mounts the token middleware on top of the management-key auth, so
// when KEEPER sets a token both credentials are required.
const ingestTokenHeader = "X-Trace-Ingest-Token"

// defaultIngestPath is the KEEPER trace ingest route.
const defaultIngestPath = "/api/v1/traces/ingest"

// defaultQueueSize caps in-flight trace reports.
const defaultQueueSize = 1024

// defaultRetryBuffer caps how many failed reports are held for replay.
const defaultRetryBuffer = 256

// defaultWriteTimeout bounds each individual POST to KEEPER.
const defaultWriteTimeout = 10 * time.Second

// retryInterval is the pacing between idle retry-buffer drain attempts.
const retryInterval = 5 * time.Second

// flushBudget bounds the best-effort drain on shutdown: a down KEEPER must
// not stall the process beyond this, and traces are best-effort — the
// remainder is dropped, not retried.
const flushBudget = 5 * time.Second

// Reporter implements coreusage.Plugin: it turns one usage record (a
// completed gateway model call) into one KEEPER trace span, on an
// asynchronous off-path — a lost or slow trace report must never block or
// fail the model response.
//
// Pipeline: HandleUsage (non-blocking enqueue) → worker → POST KEEPER.
// On failure the report moves to a bounded in-memory retry buffer and is
// replayed on the next idle tick; overflow drops the oldest pending report
// and bumps a counter. The retry buffer is process-local: a restart loses
// pending reports by design (KISS — traces are a best-effort observability
// channel, not the billing source of truth; usage events remain the
// authoritative record).
type Reporter struct {
	// ingestURL is hot-swappable (SetIngestURL): a KEEPER relocation
	// mid-run does not require a gateway restart. Read atomically on
	// every send.
	ingestURL    atomic.Value // string
	keeperKey    string
	ingestToken  string
	client       *http.Client
	writeTimeout time.Duration

	queue chan pendingReport
	retry chan pendingReport

	// stopped gates post-shutdown enqueues: once Stop has run, a late
	// HandleUsage drops instead of sending onto the abandoned channels.
	stopped atomic.Bool

	// startMu guards cancel/started; Start and Stop may race, and a Stop
	// that runs before any Start must be a no-op, not a wait on a done
	// channel that no worker will ever close.
	startMu sync.Mutex
	cancel  context.CancelFunc
	started bool
	done    chan struct{}

	enqueued atomic.Int64
	sent     atomic.Int64
	dropped  atomic.Int64
	retried  atomic.Int64
	failed   atomic.Int64
}

// pendingReport is one queued trace ingest payload.
type pendingReport struct {
	body   []byte
	header string // W3C traceparent echo, may be empty
}

// ReporterConfig configures a Reporter.
type ReporterConfig struct {
	// KeeperURL is the KEEPER base URL, e.g. http://127.0.0.1:4320.
	KeeperURL string
	// ManagementKey is the shared X-CPA-Management-Key value.
	ManagementKey string
	// IngestToken is the dedicated KEEPER trace-ingest token, sent as
	// X-Trace-Ingest-Token when KEEPER's ingest runs behind a token.
	IngestToken string
	// QueueSize caps in-flight reports; <=0 uses defaultQueueSize.
	QueueSize int
	// RetryBuffer caps pending retried reports; <=0 uses defaultRetryBuffer.
	RetryBuffer int
	// WriteTimeout bounds each POST; <=0 uses defaultWriteTimeout.
	WriteTimeout time.Duration
	// IngestPath overrides the KEEPER ingest path.
	IngestPath string
	// HTTPClient is injected for tests; nil builds one from WriteTimeout.
	HTTPClient *http.Client
}

// NewReporter builds a Reporter. Call Start before enqueuing and Stop on
// shutdown; both are safe to call multiple times.
func NewReporter(cfg ReporterConfig) *Reporter {
	if cfg.IngestPath == "" {
		cfg.IngestPath = defaultIngestPath
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultQueueSize
	}
	if cfg.RetryBuffer <= 0 {
		cfg.RetryBuffer = defaultRetryBuffer
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = defaultWriteTimeout
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: cfg.WriteTimeout}
	}
	ingestURL := trimRightSlash(cfg.KeeperURL) + cfg.IngestPath
	reporter := &Reporter{
		keeperKey:    cfg.ManagementKey,
		ingestToken:  cfg.IngestToken,
		client:       client,
		writeTimeout: cfg.WriteTimeout,
		queue:        make(chan pendingReport, cfg.QueueSize),
		retry:        make(chan pendingReport, cfg.RetryBuffer),
		done:         make(chan struct{}),
	}
	reporter.ingestURL.Store(ingestURL)
	return reporter
}

// ingestURLValue returns the current ingest URL.
func (r *Reporter) ingestURLValue() string {
	if value := r.ingestURL.Load(); value != nil {
		return value.(string)
	}
	return ""
}

// SetIngestURL relocates the KEEPER ingest endpoint at runtime without a
// restart. Reports in flight keep their original URL; the switch applies
// to the next send.
func (r *Reporter) SetIngestURL(url string) {
	if r == nil {
		return
	}
	r.ingestURL.Store(url)
}

func trimRightSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

// HandleUsage implements coreusage.Plugin. It must never block: an enqueue
// that would overflow drops the report (a missed trace must not stall the
// usage dispatcher, which runs on the model-call completion path).
func (r *Reporter) HandleUsage(ctx context.Context, record coreusage.Record) {
	if r == nil || r.stopped.Load() {
		return
	}
	payload, ok := BuildIngestRequest(ctx, record)
	if !ok {
		return
	}
	body, err := payload.Encode()
	if err != nil {
		log.WithError(err).Debug("tracer: failed to encode ingest payload")
		return
	}
	report := pendingReport{body: body, header: payload.Traceparent}
	select {
	case r.queue <- report:
		r.enqueued.Add(1)
	default:
		// Queue full: drop the report. The model response is already being
		// delivered; tracing degrades, the call does not.
		r.dropped.Add(1)
		log.Warnf("tracer: report queue full (%d), dropping trace report", cap(r.queue))
	}
}

// Start launches the delivery worker. Safe to call multiple times.
func (r *Reporter) Start() {
	if r == nil {
		return
	}
	r.startMu.Lock()
	defer r.startMu.Unlock()
	if r.started || r.stopped.Load() {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.started = true
	r.done = make(chan struct{})
	go r.worker(ctx)
	log.Infof("tracer: auto-trace reporting started (ingest=%s queue=%d retry=%d)",
		r.ingestURLValue(), cap(r.queue), cap(r.retry))
}

// Stop shuts the worker down: it marks the reporter stopped (late enqueues
// are dropped), cancels the worker, and waits for its best-effort flush.
// The channels are abandoned, not closed, so a concurrent HandleUsage that
// slips through cannot panic on a closed-channel send.
func (r *Reporter) Stop() {
	if r == nil {
		return
	}
	r.startMu.Lock()
	if !r.started {
		// No worker was ever started: nothing to flush, just close the
		// door so any enqueued report drops.
		r.stopped.Store(true)
		r.startMu.Unlock()
		return
	}
	r.started = false
	done := r.done
	r.startMu.Unlock()
	r.stopped.Store(true)
	if r.cancel != nil {
		r.cancel()
	}
	<-done
}

// Stats returns delivery counters for observability.
func (r *Reporter) Stats() (enqueued, sent, dropped, retried, failed int64) {
	if r == nil {
		return
	}
	return r.enqueued.Load(), r.sent.Load(), r.dropped.Load(), r.retried.Load(), r.failed.Load()
}

// worker drains the live queue immediately and replays the retry buffer on
// idle ticks. A live report is delivered without waiting, so the async path
// adds no latency to the model response; the retry buffer is paced so a
// down KEEPER never busy-loops.
func (r *Reporter) worker(ctx context.Context) {
	defer close(r.done)
	ticker := time.NewTicker(retryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			r.flushWithDeadline()
			return
		case report := <-r.queue:
			r.deliver(report)
		case <-ticker.C:
			r.drainRetryOnce()
		}
	}
}

// flushWithDeadline best-effort sends queued and pending-retry reports,
// bounded by flushBudget so a down KEEPER cannot stall shutdown. Traces
// are best-effort: whatever is left after the budget is dropped. Returns
// immediately when nothing is pending.
func (r *Reporter) flushWithDeadline() {
	deadline := time.Now().Add(flushBudget)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return
		}
		select {
		case report := <-r.queue:
			if !r.sendWithBudget(report, remaining) {
				r.failed.Add(1)
			}
		case report := <-r.retry:
			if !r.sendWithBudget(report, remaining) {
				r.failed.Add(1)
			}
		default:
			// Both channels empty: nothing left to flush.
			return
		}
	}
}

// sendWithBudget sends one report with a per-attempt timeout capped by the
// remaining flush budget. remaining <= 0 means "no cap" (steady-state path).
func (r *Reporter) sendWithBudget(report pendingReport, remaining time.Duration) bool {
	budget := r.writeTimeout
	if remaining > 0 && remaining < budget {
		budget = remaining
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.ingestURLValue(), bytes.NewReader(report.body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	if r.keeperKey != "" {
		req.Header.Set(managementKeyHeader, r.keeperKey)
	}
	if r.ingestToken != "" {
		req.Header.Set(ingestTokenHeader, r.ingestToken)
	}
	if report.header != "" {
		req.Header.Set(traceparentHeader, report.header)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		log.Debugf("tracer: ingest failed: %v", err)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		log.Debugf("tracer: ingest returned %d", resp.StatusCode)
		return false
	}
	r.sent.Add(1)
	return true
}

// send delivers one report on the steady-state (uncapped) path.
func (r *Reporter) send(report pendingReport) bool {
	return r.sendWithBudget(report, 0)
}

// deliver sends one report; on failure it parks it in the retry buffer.
func (r *Reporter) deliver(report pendingReport) {
	if r.send(report) {
		return
	}
	r.failed.Add(1)
	select {
	case r.retry <- report:
	default:
		// Retry buffer full: drop the oldest pending report to keep the
		// newest — traces decay in value with age. The buffer is full,
		// hence non-empty, so this receive cannot block.
		<-r.retry
		r.retry <- report
		r.dropped.Add(1)
	}
}

// drainRetryOnce replays at most one pending report when KEEPER is idle.
func (r *Reporter) drainRetryOnce() {
	select {
	case report := <-r.retry:
		if r.send(report) {
			r.retried.Add(1)
			return
		}
		// Failed again: leave it for a later tick (re-enqueue oldest-safe).
		select {
		case r.retry <- report:
		default:
			<-r.retry
			r.retry <- report
			r.dropped.Add(1)
		}
	default:
	}
}
