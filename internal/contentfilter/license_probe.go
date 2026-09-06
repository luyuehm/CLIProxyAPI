package contentfilter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"
)

// Package contentfilter: license_probe.go implements RIC-476 — CPA ↔ KEEPER
// License 健康联动探测。
//
// CPA 复用既有 KEEPER 控制面通道（CPA_CONTENT_FILTER_AUDIT_KEEPER_URL +
// CPA_CONTENT_FILTER_AUDIT_KEEPER_KEY，即 KEEPER 的 X-CPA-Management-Key
// 共享密钥）周期读取 GET {keeper}/api/v1/license/status。授权未激活/失效/
// 过期/吊销/设备不匹配/错误时，CPA 将企业功能(content-filter / audit-export)
// 显式降级并记录 ERROR 告警，而不是静默失败。
//
// RIC-476 背景：KEEPER = 管理面/授权宿主，CPA = 数据面/网关。保持两进程与
// 仓库独立，通过 License + 管理密钥做控制面联动。本探针是数据面对授权健康
// 的消费端；管理面侧的统一授权监控告警由 KEEPER 的 alert 引擎
// (license_invalid 指标)负责。

const (
	// defaultLicenseProbeInterval is how often CPA re-checks KEEPER license
	// status at runtime. Startup always probes once synchronously; the
	// background loop keeps the verdict fresh for the degradation path.
	defaultLicenseProbeInterval = 60 * time.Second
	// defaultLicenseProbeTimeout bounds a single status probe request.
	defaultLicenseProbeTimeout = 10 * time.Second

	// EnvLicenseProbeInterval overrides the probe interval (seconds).
	EnvLicenseProbeInterval = "CPA_CONTENT_FILTER_LICENSE_PROBE_INTERVAL_SECONDS"
)

// licenseStatusView mirrors the subset of KEEPER's LicenseStatus JSON that
// CPA needs for a verdict and for the degradation log/headers. Unknown fields
// in the response are ignored.
type licenseStatusView struct {
	Enabled   bool     `json:"enabled"`
	Status    string   `json:"status"`
	Message   string   `json:"message,omitempty"`
	Features  []string `json:"features,omitempty"`
	ExpiresAt string   `json:"expires_at,omitempty"`
	Mode      string   `json:"mode"`
	Revoked   bool     `json:"revoked,omitempty"`
	LicenseID string   `json:"license_id,omitempty"`
}

// isOK reports whether a KEEPER status view means "license active enough to
// run CPA enterprise features".
//
//   - "valid": OK — the configured license passes local evaluation.
//   - "unlicensed" (enabled=false): OK for backward compatibility — a KEEPER
//     with no license subsystem at all (free tier) reports this, and there is
//     nothing to link to, so CPA keeps its current open behavior.
//   - Every other verdict (expired, revoked, invalid, device_mismatch, error,
//     and a configured-but-not-activated license which evaluates as invalid)
//     is NOT OK — the issue's requirement is to degrade loudly on invalid or
//     not-activated licenses, not to silently run enterprise features.
func (v licenseStatusView) isOK() bool {
	switch v.Status {
	case "valid":
		return true
	case "unlicensed":
		// Free tier: only when KEEPER explicitly reports the license subsystem
		// is absent (enabled=false). A configured license that fails to verify
		// reports a failure verdict instead, so this stays backward compatible.
		return !v.Enabled
	default:
		return false
	}
}

// LicenseProbe is the CPA-side consumer of KEEPER's license health. It is
// safe for concurrent use: the background poller writes atomics and the
// request path only reads them, so the degradation check never blocks a
// request on network I/O.
//
// The verdict is latched: only an authoritative response (a successful
// /license/status body, or an HTTP 4xx/5xx like 401) flips it. A transient
// network error (connection refused / timeout — e.g. KEEPER restarting) keeps
// the last verdict, and a never-verified probe stays open (valid) so a
// KEEPER that starts after CPA does not degrade CPA at boot.
type LicenseProbe struct {
	baseURL  string
	key      string
	client   *http.Client
	interval time.Duration

	valid atomic.Bool
	view  atomic.Value // licenseStatusView

	probes   atomic.Uint64
	failures atomic.Uint64

	stateMu   sync.Mutex
	lastValid *bool
	cancel    context.CancelFunc
	loopOnce  sync.Once
	loopWG    sync.WaitGroup
}

// NewLicenseProbe builds a probe against a KEEPER base URL with the shared
// management key. It is the test/embedding constructor; the production path
// uses NewLicenseProbeFromEnv.
func NewLicenseProbe(baseURL, key string) *LicenseProbe {
	return &LicenseProbe{
		baseURL:  strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		key:      strings.TrimSpace(key),
		interval: defaultLicenseProbeInterval,
		client:   &http.Client{Timeout: defaultLicenseProbeTimeout},
	}
}

// NewLicenseProbeFromEnv builds a probe from the shared KEEPER control-plane
// env vars. It returns nil when no KEEPER source is configured (no URL or no
// management key) — in that case there is no license subsystem to probe and
// the caller keeps current behavior (open).
func NewLicenseProbeFromEnv() *LicenseProbe {
	url := strings.TrimSpace(os.Getenv(EnvKEEPERAuditURL))
	if url == "" {
		// Dev 默认：从 CPA_BASE_URL 推导 4320 端口（与 audit 通道一致）。
		if base := strings.TrimSpace(os.Getenv(EnvCPAURL)); base != "" {
			url = deriveKEEPERAuditURL(base)
		}
	}
	key := strings.TrimSpace(os.Getenv(EnvKEEPERManagementKey))
	if url == "" || key == "" {
		return nil
	}
	p := NewLicenseProbe(url, key)
	if v := strings.TrimSpace(os.Getenv(EnvLicenseProbeInterval)); v != "" {
		if sec, err := strconv.Atoi(v); err == nil && sec > 0 {
			p.interval = time.Duration(sec) * time.Second
		}
	}
	return p
}

// sharedLicenseProbe lazily builds and starts a single probe shared by the
// content-filter middleware and the audit-export endpoint. Both ServerOption
// and ExportServerOption are independent options that may or may not be
// mounted; a package-level singleton avoids two pollers hitting KEEPER.
//
// A mutex (not sync.Once) guards the singleton so tests can re-pin the value
// (sync.Once is not copyable and can't be reset for injection).
var (
	licenseProbeMu  sync.Mutex
	licenseProbeVal *LicenseProbe
)

func sharedLicenseProbe() *LicenseProbe {
	licenseProbeMu.Lock()
	defer licenseProbeMu.Unlock()
	if licenseProbeVal == nil {
		licenseProbeVal = NewLicenseProbeFromEnv()
		if licenseProbeVal != nil {
			licenseProbeVal.Start()
		}
	}
	return licenseProbeVal
}

// setSharedLicenseProbe pins the shared probe singleton. Test hook only; nil
// resets so the next sharedLicenseProbe re-initialises from env.
func setSharedLicenseProbe(p *LicenseProbe) {
	licenseProbeMu.Lock()
	defer licenseProbeMu.Unlock()
	licenseProbeVal = p
}

// Start performs a synchronous initial probe and begins the background poll
// loop. It is idempotent. The synchronous first probe means the very first
// request served does not see a never-probed (unknown) verdict.
func (p *LicenseProbe) Start() *LicenseProbe {
	if p == nil {
		return nil
	}
	p.loopOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		p.cancel = cancel
		// Synchronous initial probe: only an authoritative failure here should
		// degrade CPA at boot; a transient error leaves the probe open.
		p.CheckOnce(ctx)
		p.loopWG.Add(1)
		go p.loop(ctx)
	})
	return p
}

// Stop ends the poll loop and waits for it to exit.
func (p *LicenseProbe) Stop() {
	if p == nil || p.cancel == nil {
		return
	}
	p.cancel()
	p.loopWG.Wait()
}

// loop polls KEEPER on the configured interval.
func (p *LicenseProbe) loop(ctx context.Context) {
	defer p.loopWG.Done()
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.CheckOnce(ctx)
		}
	}
}

// Valid reports whether the license is currently OK. A nil probe reports true
// (no license subsystem configured → open), preserving backward compatibility
// for callers that pass no probe.
func (p *LicenseProbe) Valid() bool {
	if p == nil {
		return true
	}
	return p.valid.Load()
}

// Status returns the latest license view (empty when never probed).
func (p *LicenseProbe) Status() licenseStatusView {
	if p == nil {
		return licenseStatusView{}
	}
	if v, ok := p.view.Load().(licenseStatusView); ok {
		return v
	}
	return licenseStatusView{}
}

// Stats returns probe counters for observability: total probes and
// authoritative failure verdicts (non-OK body or HTTP 4xx/5xx).
func (p *LicenseProbe) Stats() (probes, failures uint64) {
	if p == nil {
		return 0, 0
	}
	return p.probes.Load(), p.failures.Load()
}

// CheckOnce performs a single synchronous probe and updates the cached
// verdict. It is safe for concurrent use. It returns whether the license is
// currently OK (after latching — see LicenseProbe).
func (p *LicenseProbe) CheckOnce(ctx context.Context) bool {
	p.probes.Add(1)
	view, perr := p.fetchStatus(ctx)
	if perr != nil {
		if perr.authoritative {
			// HTTP 4xx/5xx (e.g. 401 wrong management key) or an unparseable
			// body: a definitive failure. Degrade loudly.
			p.updateState(false, licenseStatusView{Enabled: true, Status: "error", Message: perr.err.Error()})
		} else {
			// Transient network error: latch — keep the last verdict. A never-
			// verified probe stays open (Valid()=true), so KEEPER starting
			// after CPA does not degrade CPA.
			p.recordTransient(perr.err)
		}
		return p.Valid()
	}
	ok := view.isOK()
	p.updateState(ok, view)
	return ok
}

// probeError distinguishes an authoritative failure (HTTP status / bad body)
// from a transient one (network/timeout).
type probeError struct {
	err           error
	authoritative bool
}

func (e *probeError) Error() string { return e.err.Error() }

// fetchStatus GETs KEEPER's /api/v1/license/status with the shared management
// key. A non-2xx response (e.g. 401 when the key is wrong) is an authoritative
// failure — exactly the "KEEPER license 401/expired" alert the issue asks for.
// A transport error (connection refused / timeout) is transient.
func (p *LicenseProbe) fetchStatus(ctx context.Context) (licenseStatusView, *probeError) {
	url := p.baseURL + "/api/v1/license/status"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return licenseStatusView{}, &probeError{err: fmt.Errorf("license probe: build request: %w", err), authoritative: true}
	}
	req.Header.Set("X-CPA-Management-Key", p.key)

	client := p.client
	if client == nil {
		client = &http.Client{Timeout: defaultLicenseProbeTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		// Transport-level: transient (KEEPER restarting, network blip).
		return licenseStatusView{}, &probeError{err: fmt.Errorf("license probe: get: %w", err)}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return licenseStatusView{}, &probeError{err: fmt.Errorf("license probe: status=%d body=%s",
			resp.StatusCode, strings.TrimSpace(string(body))), authoritative: true}
	}
	var view licenseStatusView
	if err := json.NewDecoder(resp.Body).Decode(&view); err != nil {
		return licenseStatusView{}, &probeError{err: fmt.Errorf("license probe: decode: %w", err), authoritative: true}
	}
	return view, nil
}

// recordTransient logs a throttled warning and leaves the verdict latched.
func (p *LicenseProbe) recordTransient(err error) {
	// Log at debug to avoid flooding on every interval during a KEEPER outage;
	// the transition logging in updateState only fires on verdict changes.
	logger.WithError(err).Debug("license probe: KEEPER unreachable (transient, keeping last verdict)")
}

// updateState stores the verdict and logs on status transitions only — the
// background poller runs every interval, so logging every identical verdict
// would flood the gateway log. The first verdict and any change are logged
// loudly (ERROR on invalid, INFO on recovery).
func (p *LicenseProbe) updateState(ok bool, view licenseStatusView) {
	p.valid.Store(ok)
	p.view.Store(view)

	p.stateMu.Lock()
	prev := p.lastValid
	p.lastValid = &ok
	p.stateMu.Unlock()

	if !ok {
		p.failures.Add(1)
	}

	fields := logrus.Fields{
		"license_status":  view.Status,
		"license_mode":    view.Mode,
		"license_message": view.Message,
		"expires_at":      view.ExpiresAt,
		"revoked":         view.Revoked,
	}
	switch {
	case prev == nil && ok:
		logger.WithFields(fields).Info("KEEPER license probe: valid")
	case prev == nil && !ok:
		logger.WithFields(fields).Error("KEEPER license not valid; CPA enterprise features degraded (content-filter/audit-export blocked)")
	case ok && prev != nil && !*prev:
		logger.WithFields(fields).Info("KEEPER license recovered: valid")
	case !ok && prev != nil && *prev:
		logger.WithFields(fields).Error("KEEPER license lost; CPA enterprise features degraded (content-filter/audit-export blocked)")
	}
}
