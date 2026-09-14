package auth

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	log "github.com/sirupsen/logrus"
)

// healthCheckProbe carries the minimal probe inputs for one endpoint.
type healthCheckProbe struct {
	auth        *Auth
	executor    ProviderExecutor
	probeURL    string
	probePath   string
	lastHealthy bool
}

// StartOfflineHealthCheck launches the D4 proactive endpoint health-check loop.
//
// It probes each configured OpenAI-compatible endpoint on an interval and
// records availability through MarkResult, so unhealthy endpoints are excluded
// from selection (via the existing cooldown/availability machinery) and recover
// automatically when the endpoint comes back. The loop is stopped by cancelling
// the supplied context.
//
// The loop only probes endpoints that are eligible under the current offline
// policy (probe-only-local in offline mode), mirroring the request-time filter.
func (m *Manager) StartOfflineHealthCheck(parent context.Context) {
	if m == nil {
		return
	}
	probesMu := sync.Mutex{}
	var probes []healthCheckProbe

	rebuild := func() {
		cfg := m.runtimeConfigSnapshot()
		if cfg == nil || !cfg.OfflineHealthCheckEnabled() {
			probesMu.Lock()
			probes = nil
			probesMu.Unlock()
			return
		}
		probeOnlyLocal := cfg.OfflineHealthCheckProbeOnlyLocal()
		path := cfg.OfflineHealthCheckPath()
		newProbes := make([]healthCheckProbe, 0)

		m.mu.RLock()
		for _, auth := range m.auths {
			if auth == nil || auth.Disabled {
				continue
			}
			if auth.Attributes == nil {
				continue
			}
			baseURL := strings.TrimSpace(auth.Attributes["base_url"])
			if baseURL == "" {
				continue
			}
			// Only OpenAI-compatibility configured endpoints participate.
			if strings.TrimSpace(auth.Attributes["compat_name"]) == "" &&
				strings.TrimSpace(auth.Attributes["provider_key"]) == "" {
				continue
			}
			if probeOnlyLocal && !auth.IsLocalEndpoint() {
				continue
			}
			executor, ok := m.executors[auth.Provider]
			if !ok || executor == nil {
				continue
			}
			probeURL := strings.TrimSuffix(baseURL, "/") + path
			newProbes = append(newProbes, healthCheckProbe{
				auth:        auth.Clone(),
				executor:    executor,
				probeURL:    probeURL,
				probePath:   path,
				lastHealthy: true,
			})
		}
		m.mu.RUnlock()

		probesMu.Lock()
		probes = newProbes
		probesMu.Unlock()
	}

	probeOnce := func(ctx context.Context) {
		cfg := m.runtimeConfigSnapshot()
		timeout := internalconfig.DefaultOfflineHealthCheckTimeout
		if cfg != nil {
			timeout = cfg.OfflineHealthCheckTimeout()
		}
		probesMu.Lock()
		snapshot := make([]healthCheckProbe, len(probes))
		copy(snapshot, probes)
		probesMu.Unlock()

		for i := range snapshot {
			if ctx.Err() != nil {
				return
			}
			p := &snapshot[i]
			probeCtx, cancel := context.WithTimeout(ctx, timeout)
			req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, p.probeURL, nil)
			if err != nil {
				cancel()
				continue
			}
			healthy := false
			resp, errReq := p.executor.HttpRequest(probeCtx, p.auth, req)
			if errReq == nil && resp != nil {
				// Drain and close the body to reuse the connection.
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				healthy = resp.StatusCode >= 200 && resp.StatusCode < 300
			}
			cancel()

			if healthy == p.lastHealthy {
				continue
			}
			p.lastHealthy = healthy
			result := Result{
				AuthID:     p.auth.ID,
				Provider:   p.auth.Provider,
				Model:      "",
				RouteModel: "",
				Success:    healthy,
			}
			if !healthy {
				result.Error = &Error{
					Code:       "endpoint_unhealthy",
					Message:    "health check failed for endpoint",
					HTTPStatus: http.StatusServiceUnavailable,
				}
			}
			if healthy {
				log.Infof("offline health check: endpoint %s healthy (auth %s)", p.probeURL, util.HideAPIKey(p.auth.ID))
			} else {
				log.Warnf("offline health check: endpoint %s unhealthy (auth %s)", p.probeURL, util.HideAPIKey(p.auth.ID))
			}
			m.MarkResult(ctx, result)
		}
	}

	// Initial build and probe.
	rebuild()
	probeOnce(parent)

	interval := internalconfig.DefaultOfflineHealthCheckInterval
	if cfg := m.runtimeConfigSnapshot(); cfg != nil {
		interval = cfg.OfflineHealthCheckInterval()
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-parent.Done():
			log.Info("offline health check: stopped")
			return
		case <-ticker.C:
			rebuild()
			probeOnce(parent)
		}
	}
}
