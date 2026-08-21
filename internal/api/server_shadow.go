package api

import (
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/shadow"
)

// convertShadowConfig translates the YAML shadow configuration into the
// runtime shadow engine configuration.
func convertShadowConfig(cfg config.ShadowConfig) shadow.Config {
	out := shadow.Config{
		Enabled:     cfg.Enabled,
		QueueSize:   cfg.QueueSize,
		WorkerCount: cfg.WorkerCount,
	}
	if cfg.TimeoutSeconds > 0 {
		out.Timeout = time.Duration(cfg.TimeoutSeconds) * time.Second
	}

	for _, m := range cfg.Mirrors {
		out.Mirrors = append(out.Mirrors, shadow.MirrorConfig{
			Model:      m.Model,
			Candidate:  m.Candidate,
			Ratio:      m.Ratio,
			Endpoint:   m.Endpoint,
			APIKey:     m.APIKey,
			Headers:    m.Headers,
			UserHeader: m.UserHeader,
		})
	}

	for _, c := range cfg.Canaries {
		out.Canaries = append(out.Canaries, shadow.CanaryConfig{
			Model:        c.Model,
			Candidate:    c.Candidate,
			Weight:       c.Weight,
			Provider:     c.Provider,
			Headers:      c.Headers,
			UserIDHeader: c.UserIDHeader,
			UserIDs:      c.UserIDs,
		})
	}
	return out
}
