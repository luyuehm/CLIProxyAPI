package config

import "time"

// AlertEnterpriseConfig holds the enterprise alert notification configuration.
// It configures Feishu/DingTalk/WeCom webhook robot notification channels.
type AlertEnterpriseConfig struct {
	// Channels maps IM channel names to their webhook configuration.
	// Supported names: "feishu", "dingtalk", "wecom".
	Channels map[string]AlertChannelConfig `yaml:"channels" json:"channels"`
	// SilencePeriod is the minimum interval between duplicate alerts (e.g. "5m").
	// Default: 5m (5 minutes).
	SilencePeriod time.Duration `yaml:"silence-period,omitempty" json:"silence-period,omitempty"`
}

// AlertChannelConfig holds the per-channel webhook endpoint configuration.
type AlertChannelConfig struct {
	// WebhookURL is the incoming robot webhook URL.
	WebhookURL string `yaml:"webhook-url" json:"webhook-url"`
	// SignSecret is the HMAC-SHA256 signing secret (DingTalk uses this for
	// webhook signing). Optional per channel.
	SignSecret string `yaml:"sign-secret,omitempty" json:"sign-secret,omitempty"`
	// Enabled controls whether this channel is active.
	Enabled bool `yaml:"enabled" json:"enabled"`
}
