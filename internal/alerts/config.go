package alerts

import (
	"strings"
	"time"
)

// Defaults applied when the corresponding config value is empty or invalid.
const (
	defaultCheckInterval = time.Minute
	defaultHTTPTimeout   = 5 * time.Second
)

// Config is the user-facing alerts configuration, nested under the "alerts"
// key in config.yaml.
type Config struct {
	// Enabled toggles the whole alerts subsystem. When false, usage records
	// are ignored and no rule is evaluated.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// CheckInterval is the rule evaluation interval as a Go duration string
	// (e.g. "1m", "30s"). Defaults to "1m".
	CheckInterval string `yaml:"check-interval" json:"check-interval"`
	// HTTPTimeout bounds each webhook push as a Go duration string.
	// Defaults to "5s".
	HTTPTimeout string `yaml:"http-timeout" json:"http-timeout"`
	// FeishuWebhookURL enables the Feishu channel when non-empty.
	FeishuWebhookURL string `yaml:"feishu-webhook-url" json:"feishu-webhook-url"`
	// DingTalkWebhookURL enables the DingTalk channel when non-empty.
	DingTalkWebhookURL string `yaml:"dingtalk-webhook-url" json:"dingtalk-webhook-url"`
	// DingTalkSecret signs DingTalk requests when non-empty.
	DingTalkSecret string `yaml:"dingtalk-secret" json:"dingtalk-secret"`
	// WeComWebhookURL enables the WeCom channel when non-empty.
	WeComWebhookURL string `yaml:"wecom-webhook-url" json:"wecom-webhook-url"`
	// Rules is the ordered set of alert rules evaluated every interval.
	Rules []Rule `yaml:"rules" json:"rules"`
}

// Interval parses CheckInterval, falling back to the default.
func (c Config) Interval() time.Duration {
	return parseDuration(c.CheckInterval, defaultCheckInterval)
}

// Timeout parses HTTPTimeout, falling back to the default.
func (c Config) Timeout() time.Duration {
	return parseDuration(c.HTTPTimeout, defaultHTTPTimeout)
}

func parseDuration(value string, fallback time.Duration) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return fallback
	}
	return duration
}

// Normalized returns a copy of the config with whitespace trimmed, durations
// canonicalized, and rule defaults applied. Rules that are not evaluable are
// still kept so the management API can report what was configured.
func (c Config) Normalized() Config {
	out := c
	out.CheckInterval = c.Interval().String()
	out.HTTPTimeout = c.Timeout().String()
	out.FeishuWebhookURL = strings.TrimSpace(c.FeishuWebhookURL)
	out.DingTalkWebhookURL = strings.TrimSpace(c.DingTalkWebhookURL)
	out.DingTalkSecret = strings.TrimSpace(c.DingTalkSecret)
	out.WeComWebhookURL = strings.TrimSpace(c.WeComWebhookURL)

	rules := make([]Rule, 0, len(c.Rules))
	for _, rule := range c.Rules {
		rules = append(rules, rule.normalized())
	}
	out.Rules = rules
	return out
}

func (r Rule) normalized() Rule {
	r.Name = strings.TrimSpace(r.Name)
	r.Target = strings.TrimSpace(r.Target)
	if r.Severity == "" {
		r.Severity = SeverityWarning
	}
	r.Cooldown = strings.TrimSpace(r.Cooldown)
	return r
}
