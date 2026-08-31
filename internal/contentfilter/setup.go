package contentfilter

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/api"
)

// Environment variables controlling the realtime content filter. All are
// optional; sensible defaults point at the KEEPER deployment used by the
// gateway (container enterprise-keeper-cpa-usage-keeper-1, volume path
// /var/lib/docker/volumes/enterprise-keeper_keeper-data/_data).
const (
	// EnvEnabled forces the filter on/off. "1"|"true"|"yes"|"on" enables,
	// "0"|"false"|"no"|"off" disables. Unset means auto-detect.
	EnvEnabled = "CPA_CONTENT_FILTER_ENABLED"
	// EnvHostDB overrides the host-visible KEEPER database path. When set and
	// readable, rules are read directly (read-only) from the mounted volume.
	EnvHostDB = "CPA_CONTENT_FILTER_KEEPER_HOST_DB"
	// EnvContainer overrides the KEEPER container name used for `docker cp`
	// (the fallback channel when the host volume path is unavailable).
	EnvContainer = "CPA_CONTENT_FILTER_KEEPER_CONTAINER"
	// EnvDBPath overrides the in-container SQLite database path.
	EnvDBPath = "CPA_CONTENT_FILTER_KEEPER_DB_PATH"
	// EnvIntervalSec overrides the rule refresh interval in seconds (default 30).
	EnvIntervalSec = "CPA_CONTENT_FILTER_INTERVAL_SECONDS"
	// EnvKEEPERAuditURL 指向 KEEPER 暴露的审计 ingest 端点 base URL
	//（例如 http://127.0.0.1:4320）。RIC-442 之后，审计行通过 HTTP 投递
	// 给 KEEPER，KEEPER 进程以 app:app 写入自己的 app.db，CPA 不再以宿主
	// UID 直接打开 KEEPER 的 SQLite 文件。
	EnvKEEPERAuditURL = "CPA_CONTENT_FILTER_AUDIT_KEEPER_URL"
	// EnvKEEPERManagementKey 是 CPA↔KEEPER 共享管理密钥（与 KEEPER 配置
	// 的 CPA_MANAGEMENT_KEY 一致），通过 X-CPA-Management-Key 头传递。
	EnvKEEPERManagementKey = "CPA_CONTENT_FILTER_AUDIT_KEEPER_KEY"
	// EnvCPAURL 是 KEEPER 访问 CPA 的 base URL（来自 CPA_BASE_URL），用于
	// 自动推导默认 audit URL：取该值 + ":4320" 替换端口作为 dev 默认。
	// 生产应显式配置 EnvKEEPERAuditURL。
	EnvCPAURL = "CPA_BASE_URL"
)

// ServerOption returns an api.ServerOption that installs the realtime content
// filter middleware, constructing the rule syncer (initial load + background
// polling) and audit writer at server construction time. It returns nil when
// the filter is disabled by configuration and no KEEPER source is reachable —
// callers should skip the option in that case.
//
// The returned option mounts through the pre-reserved api.WithMiddleware()
// extension point; it does not modify any upstream handler or core file.
func ServerOption() api.ServerOption {
	if !enabledFromEnv() {
		logger.Warn("content filter disabled (set " + EnvEnabled + "=true and configure a KEEPER source to enable)")
		return nil
	}
	opts := optsFromEnv()
	syncer, err := NewSyncer(opts)
	if err != nil {
		logger.WithError(err).Warn("content filter not installed: cannot create syncer")
		return nil
	}
	// Background polling hot-reloads rules within one RefreshInterval without
	// restarting the gateway.
	syncer.Start()
	engine := NewEngine(true) // outbound PII uses partial masking
	audit := NewAudit(auditEnvFromSyncer(opts))
	mw := NewMiddleware(syncer, engine, audit)
	return api.WithMiddleware(mw.Handler())
}

// auditEnvFromSyncer converts a SyncerOptions into AuditEnv, sharing the
// KEEPER host / container paths and adding the queue / timeout defaults.
//
// RIC-442: 优先从 env 读取 HTTP ingest 通道配置（KEEPERAuditURL +
// KEEPERManagementKey）。任何写过 KEEPER SQLite 的字段（HostDBPath /
// ContainerName / ContainerDBPath / DockerCmd）默认留空——新部署不应让
// CPA 直接以宿主 UID 打开 KEEPER 的卷。
func auditEnvFromSyncer(s SyncerOptions) AuditEnv {
	env := AuditEnv{
		HostDBPath:           s.HostDBPath,
		ContainerName:        s.ContainerName,
		ContainerDBPath:      s.ContainerDBPath,
		DockerCmd:            s.DockerCmd,
		KEEPERAuditURL:       strings.TrimSpace(os.Getenv(EnvKEEPERAuditURL)),
		KEEPERManagementKey:  strings.TrimSpace(os.Getenv(EnvKEEPERManagementKey)),
		WriteTimeout:         DefaultDockerCopyTimeout,
	}
	if env.KEEPERAuditURL == "" {
		// Dev 默认：从 CPA_BASE_URL 推导 4320 端口。
		if base := strings.TrimSpace(os.Getenv(EnvCPAURL)); base != "" {
			env.KEEPERAuditURL = deriveKEEPERAuditURL(base)
		}
	}
	return env
}

// deriveKEEPERAuditURL 替换 base URL 的端口为 4320（KEEPER 默认端口）。
// 仅作为开发与本地回退；生产应显式配置 EnvKEEPERAuditURL。
func deriveKEEPERAuditURL(base string) string {
	u := strings.TrimRight(base, "/")
	// 简单字符串替换：找最后一个 ":" 后把端口替换。
	idx := strings.LastIndex(u, ":")
	if idx < 0 {
		return u + ":4320"
	}
	prefix := u[:idx]
	return prefix + ":4320"
}

// enabledFromEnv resolves the enable switch. An explicit env value wins;
// otherwise the filter auto-enables when a KEEPER source is configured (host
// volume path present, or an explicit container name) so bare test and dev
// environments stay quiet.
func enabledFromEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvEnabled))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	host := strings.TrimSpace(os.Getenv(EnvHostDB))
	if host == "" {
		host = DefaultHostVolumeDBPath
	}
	if _, err := os.Stat(host); err == nil {
		return true
	}
	return strings.TrimSpace(os.Getenv(EnvContainer)) != ""
}

// optsFromEnv builds the syncer options from environment configuration with
// the KEEPER defaults applied.
func optsFromEnv() SyncerOptions {
	opts := DefaultSyncerOptions()
	if h := strings.TrimSpace(os.Getenv(EnvHostDB)); h != "" {
		opts.HostDBPath = h
	}
	if c := strings.TrimSpace(os.Getenv(EnvContainer)); c != "" {
		opts.ContainerName = c
	}
	if db := strings.TrimSpace(os.Getenv(EnvDBPath)); db != "" {
		opts.ContainerDBPath = db
	}
	if s := strings.TrimSpace(os.Getenv(EnvIntervalSec)); s != "" {
		if sec, err := strconv.Atoi(s); err == nil && sec > 0 {
			opts.RefreshInterval = time.Duration(sec) * time.Second
		}
	}
	return opts
}
