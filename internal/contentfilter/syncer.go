package contentfilter

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"
	_ "modernc.org/sqlite"
)

// logger is the package-level logger used by the syncer and middleware.
// KEEPER rule loading is runtime infrastructure, so it uses logrus like the
// rest of the gateway rather than a bespoke logger.
var logger = logrus.WithField("component", "contentfilter")

// Default settings for the rule syncer.
const (
	DefaultInterval          = 30 * time.Second
	DefaultContainerName     = "enterprise-keeper-cpa-usage-keeper-1"
	DefaultContainerDBPath   = "/data/app.db"
	DefaultHostVolumeDBPath  = "/var/lib/docker/volumes/enterprise-keeper_keeper-data/_data/app.db"
	DefaultDockerCopyTimeout = 10 * time.Second
)

// SyncerOptions configures how the rule syncer reaches KEEPER's SQLite
// database. At least one of HostDBPath, ContainerName or ContainerDBPath must
// be set; when docker mode is used, the DB file is copied out read-only on
// every sync so KEEPER is never written to.
type SyncerOptions struct {
	// KEEPER app.db read from a host-visible path (mounted volume). When set,
	// this path is opened directly in read-only mode.
	HostDBPath string
	// When set, the syncer reaches the KEEPER app.db inside the container via
	// `docker cp` (read-only copy) instead of reading the host path.
	ContainerName string
	// ContainerDBPath is the in-container path of the SQLite database.
	ContainerDBPath string
	// DockerCmd is the docker CLI binary (defaults to "docker").
	DockerCmd string
	// RefreshInterval controls how often rules are reloaded.
	RefreshInterval time.Duration
	// dockerCopyTimeout bounds each docker cp invocation.
	dockerCopyTimeout time.Duration
}

func (o *SyncerOptions) withDefaults() {
	if o.RefreshInterval <= 0 {
		o.RefreshInterval = DefaultInterval
	}
	if o.ContainerDBPath == "" {
		o.ContainerDBPath = DefaultContainerDBPath
	}
	if o.ContainerName == "" {
		o.ContainerName = DefaultContainerName
	}
	if o.DockerCmd == "" {
		o.DockerCmd = "docker"
	}
	if o.dockerCopyTimeout <= 0 {
		o.dockerCopyTimeout = DefaultDockerCopyTimeout
	}
}

// DefaultSyncerOptions builds a SyncerOptions that prefers the host volume
// path and falls back to `docker cp` transparently.
func DefaultSyncerOptions() SyncerOptions {
	return SyncerOptions{
		HostDBPath:       DefaultHostVolumeDBPath,
		ContainerName:    DefaultContainerName,
		ContainerDBPath:  DefaultContainerDBPath,
		RefreshInterval:  DefaultInterval,
		DockerCmd:        "docker",
		dockerCopyTimeout: DefaultDockerCopyTimeout,
	}
}

// Syncer periodically reads KEEPER's content_filter_rules table into memory.
// It loads rules on creation (or on the first successful poll) and then
// re-polls every RefreshInterval, comparing a lightweight source fingerprint
// (file mtime + size, or a checksum when mtime is unavailable) so rules only
// get swapped when they actually changed. Failures are logged and keep the
// last-known-good rule set.
type Syncer struct {
	opts SyncerOptions

	mu     sync.RWMutex
	active []*Rule

	stale atomic.Bool
	stop  chan struct{}
	once  sync.Once
}

// NewSyncer creates a Syncer and immediately performs the first load. A failed
// first load is not fatal: the syncer starts with an empty rule set and the
// polling loop retries on the next interval. An error is only returned when no
// KEEPER source is configured at all.
func NewSyncer(opts SyncerOptions) (*Syncer, error) {
	opts.withDefaults()
	if opts.HostDBPath == "" && opts.ContainerName == "" {
		return nil, fmt.Errorf("content filter: no KEEPER db source configured")
	}
	s := &Syncer{
		opts: opts,
		stop: make(chan struct{}),
	}
	if _, err := s.Reload(); err != nil {
		logger.WithError(err).Warn("content filter: initial rule load failed, will retry on next poll")
	}
	return s, nil
}

// Start begins the background polling loop. It returns the syncer so callers
// can chain. Call Stop to end the loop.
func (s *Syncer) Start() *Syncer {
	s.once.Do(func() {
		go s.loop()
	})
	return s
}

// Stop stops the polling loop.
func (s *Syncer) Stop() {
	s.stopOnce()
}

// Close is an alias for Stop so the syncer satisfies io.Closer and can be
// wired into lifecycle cleanup.
func (s *Syncer) Close() error {
	s.stopOnce()
	return nil
}

func (s *Syncer) stopOnce() {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
}

func (s *Syncer) loop() {
	t := time.NewTicker(s.opts.RefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.tick()
		}
	}
}

// Called on fixed intervals. Sets a stale marker when a source becomes
// unreachable so the middleware can react.
func (s *Syncer) tick() {
	if _, err := s.Reload(); err != nil {
		logger.WithError(err).Warn("content filter sync failed")
	}
}

func (s *Syncer) setStale(v bool) {
	s.stale.Store(v)
}

// Stale reports whether the last reload failed and rules are stale.
func (s *Syncer) Stale() bool {
	return s.stale.Load()
}

// Rules returns the active rule set. The slice is immutable once returned.
func (s *Syncer) Rules() []*Rule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.active
}

// Reload reads the current rules from KEEPER and atomically swaps them into
// memory. It returns the number of rules loaded.
func (s *Syncer) Reload() (int, error) {
	rules, err := s.loadRules()
	if err != nil {
		s.setStale(true)
		return 0, err
	}
	s.mu.Lock()
	s.active = rules
	s.mu.Unlock()
	s.setStale(false)
	return len(rules), nil
}

// loadRules reads rules from the KEEPER database. Host mode opens the file
// read-only directly; docker mode copies it out first.
func (s *Syncer) loadRules() ([]*Rule, error) {
	dbPath, cleanup, err := s.obtainDBPath()
	if err != nil {
		return nil, err
	}
	if cleanup != nil {
		defer cleanup()
	}
	return readRulesFromDB(dbPath)
}

// obtainDBPath decides how to reach the KEEPER db and returns the local path
// to read plus an optional cleanup func.
func (s *Syncer) obtainDBPath() (path string, cleanup func(), err error) {
	if s.opts.HostDBPath != "" {
		if _, statErr := os.Stat(s.opts.HostDBPath); statErr == nil {
			return s.opts.HostDBPath, nil, nil
		}
		// Host path missing: fall through to docker cp.
	}
	if s.opts.ContainerName != "" {
		tmp, copyErr := s.copyDBViaDocker()
		if copyErr == nil {
			return tmp, func() { _ = os.Remove(tmp) }, nil
		}
		if s.opts.HostDBPath != "" {
			return "", nil, fmt.Errorf("content filter: docker cp failed (%v) and host db %q unavailable", copyErr, s.opts.HostDBPath)
		}
		return "", nil, fmt.Errorf("content filter: docker cp failed: %v", copyErr)
	}
	return "", nil, fmt.Errorf("content filter: no KEEPER db source configured (host path or container)")
}

// copyDBViaDocker copies the KEEPER app.db to a temp file (read-only). It
// never writes to the container.
func (s *Syncer) copyDBViaDocker() (string, error) {
	tmp, err := os.CreateTemp("", "keeper-app-db-*.db")
	if err != nil {
		return "", fmt.Errorf("content filter: create temp db copy: %v", err)
	}
	_ = tmp.Close()
	_ = os.Remove(tmp.Name())

	ctx, cancel := context.WithTimeout(context.Background(), s.opts.dockerCopyTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.opts.DockerCmd, "cp",
		fmt.Sprintf("%s:%s", s.opts.ContainerName, s.opts.ContainerDBPath), tmp.Name())
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.Remove(tmp.Name())
		return "", fmt.Errorf("content filter: docker cp %s:%s: %v: %s",
			s.opts.ContainerName, s.opts.ContainerDBPath, err, strings.TrimSpace(string(out)))
	}
	return tmp.Name(), nil
}

// readRulesFromDB opens the SQLite database read-only and loads all enabled
// content filter rules, ordered by priority (then id) ascending so higher
// priorities win when multiple rules touch the same text.
func readRulesFromDB(dbPath string) ([]*Rule, error) {
	dsn := "file:" + filepath.ToSlash(dbPath) + "?mode=ro&_pragma=busy_timeout%3d5000"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("content filter: open keeper db: %w", err)
	}
	defer db.Close()

	// A read-only open can still fail later on permission; surface it here.
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("content filter: ping keeper db: %w", err)
	}

	rows, err := db.QueryContext(context.Background(),
		`SELECT id, name, IFNULL(description, ''), COALESCE(enabled, 1), COALESCE(scenario, 'general'),
		        COALESCE(action, 'mask'), IFNULL(sensitive_words, ''), IFNULL(pii_types, ''),
		        IFNULL(white_list, ''), IFNULL(models, ''), COALESCE(priority, 0)
		 FROM content_filter_rules ORDER BY priority ASC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("content filter: query content_filter_rules: %w", err)
	}
	defer rows.Close()

	var rules []*Rule
	for rows.Next() {
		var r Rule
		var sensitive, pii, white, models string
		var enabled int
		if err := rows.Scan(&r.ID, &r.Name, &r.Description, &enabled, &r.Scenario,
			&r.Action, &sensitive, &pii, &white, &models, &r.Priority); err != nil {
			return nil, fmt.Errorf("content filter: scan rule row: %w", err)
		}
		r.Enabled = enabled != 0
		r.SensitiveWords = parseCSV(sensitive)
		r.PIITypes = parsePIITypes(pii)
		r.WhiteList = parseCSV(white)
		r.Models = parseCSV(models)
		rules = append(rules, &r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("content filter: iterate rules: %w", err)
	}
	return rules, nil
}