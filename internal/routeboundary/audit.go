package routeboundary

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"
)

// auditEndpoint is the KEEPER control-plane ingest for route-boundary audit
// events (RIC-2532-A shared contract). It is mounted under the /api/v1 group
// with adminOrManagementKeyMiddleware, so the CPA authenticates with the
// shared management key.
const auditEndpoint = "/api/v1/admin/api/route-boundaries/audit"

// AuditEvent is one out-of-boundary override pass-through, destined for
// KEEPER's route-boundary audit log.
type AuditEvent struct {
	// Timestamp is when the override was granted (UTC).
	Timestamp time.Time
	// APIKey is the constrained caller key the override was granted to.
	APIKey string
	// KeyAlias is the KEEPER display alias of that key, when delivered.
	KeyAlias string
	// Model is the requested upstream model.
	Model string
	// Boundary is the key's delivered allowed-provider list at grant time.
	Boundary []string
	// OverrideProvider is the provider the single request was let through to.
	OverrideProvider string
	// Action is fixed to "override" by the shared contract.
	Action string
	// StatusCode is the final response status of the request, captured after
	// execution; zero when the status could not be observed.
	StatusCode int
}

// auditWireEvent is the JSON body KEEPER ingests (shared contract, pinned
// field names).
type auditWireEvent struct {
	Timestamp        string   `json:"timestamp"`
	APIKey           string   `json:"apiKey"`
	KeyAlias         string   `json:"keyAlias"`
	Model            string   `json:"model"`
	Boundary         []string `json:"boundary"`
	OverrideProvider string   `json:"overrideProvider"`
	Action           string   `json:"action"`
	StatusCode       int      `json:"statusCode"`
}

// AuditSink is the seam the enforcer uses to report override events. A nil
// sink disables audit posts without changing override behaviour.
type AuditSink interface {
	// Record enqueues one event for asynchronous delivery. It never blocks
	// the request path; a full queue drops the event (best-effort audit,
	// contentfilter principle: a missed audit row must never block routing).
	Record(event AuditEvent)
}

// AuditOptions configures the async audit writer.
type AuditOptions struct {
	// KeeperURL is the KEEPER control-plane base URL the audit POSTs target.
	KeeperURL string
	// ManagementKey authenticates the audit ingest with
	// X-CPA-Management-Key.
	ManagementKey string
	// QueueSize caps the in-memory event queue depth (default 256).
	QueueSize int
	// HTTPClient bounds a single audit POST (default 10s timeout).
	HTTPClient *http.Client
}

func (o *AuditOptions) withDefaults() {
	if o.QueueSize <= 0 {
		o.QueueSize = 256
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
}

// Audit is the async, single-worker writer for route-boundary audit events.
// Failures are logged and dropped; they never propagate to the request hot
// path.
type Audit struct {
	opts  AuditOptions
	queue chan AuditEvent

	stop    chan struct{}
	once    sync.Once
	stopped atomic.Bool

	enqueued atomic.Uint64
	dropped  atomic.Uint64
	written  atomic.Uint64
	failed   atomic.Uint64
}

// NewAudit constructs the writer and starts its background worker. The
// writer is fully inert (Record is a no-op) when the KEEPER URL or the
// management key is missing — a deployment without a control plane still
// grants overrides, it simply sees no audit events.
func NewAudit(opts AuditOptions) *Audit {
	if strings.TrimSpace(opts.KeeperURL) == "" || strings.TrimSpace(opts.ManagementKey) == "" {
		return nil
	}
	opts.withDefaults()
	a := &Audit{
		opts:  opts,
		queue: make(chan AuditEvent, opts.QueueSize),
		stop:  make(chan struct{}),
	}
	a.once.Do(func() { go a.loop() })
	return a
}

// Record enqueues an event. Non-blocking: when the queue is full the event
// is dropped and counted.
func (a *Audit) Record(event AuditEvent) {
	if a == nil || a.stopped.Load() {
		return
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	}
	select {
	case a.queue <- event:
		a.enqueued.Add(1)
	default:
		a.dropped.Add(1)
		logger.Warn("routeboundary audit queue full, dropping event")
	}
}

// Stats returns writer counters for observability.
func (a *Audit) Stats() (enqueued, dropped, written, failed uint64) {
	if a == nil {
		return 0, 0, 0, 0
	}
	return a.enqueued.Load(), a.dropped.Load(), a.written.Load(), a.failed.Load()
}

// Close drains the queue (best effort) and stops the worker.
func (a *Audit) Close() {
	if a == nil {
		return
	}
	if !a.stopped.CompareAndSwap(false, true) {
		return
	}
	close(a.queue)
	<-a.stop
	logger.WithFields(logrus.Fields{
		"enqueued": a.enqueued.Load(),
		"written":  a.written.Load(),
		"failed":   a.failed.Load(),
		"dropped":  a.dropped.Load(),
	}).Debug("routeboundary audit writer stopped")
}

// loop drains the queue one event at a time. The ingest endpoint is
// append-only, so per-key ordering is preserved and there is no batching to
// tune. A blocking receive is the correct stop mechanism: while the queue is
// open it waits for the next event, and after Close() closes it, every
// remaining buffered event is delivered before the receive reports
// (zero, ok=false) exactly once — that is the drain.
func (a *Audit) loop() {
	defer close(a.stop)
	for {
		event, ok := <-a.queue
		if !ok {
			return // queue closed and fully drained
		}
		if err := a.deliver(event); err != nil {
			a.failed.Add(1)
			logger.WithError(err).WithField("action", event.Action).Debug("routeboundary audit delivery failed (event dropped)")
			continue
		}
		a.written.Add(1)
	}
}

// deliver POSTs one event to KEEPER with the shared management key.
func (a *Audit) deliver(event AuditEvent) error {
	wire := auditWireEvent{
		Timestamp:        event.Timestamp.UTC().Format(time.RFC3339),
		APIKey:           event.APIKey,
		KeyAlias:         event.KeyAlias,
		Model:            event.Model,
		Boundary:         event.Boundary,
		OverrideProvider: event.OverrideProvider,
		Action:           event.Action,
		StatusCode:       event.StatusCode,
	}
	if wire.Boundary == nil {
		wire.Boundary = []string{}
	}
	payload, err := json.Marshal(wire)
	if err != nil {
		return fmt.Errorf("routeboundary: encode audit event: %w", err)
	}
	url := strings.TrimRight(a.opts.KeeperURL, "/") + auditEndpoint
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("routeboundary: build audit request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CPA-Management-Key", a.opts.ManagementKey)
	req.Header.Set("X-CPA-Usage-Keeper-Request", "fetch") // KEEPER request-intent middleware requires it on write endpoints

	client := a.opts.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("routeboundary: post audit event: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("routeboundary: audit endpoint status=%d", resp.StatusCode)
	}
	return nil
}
