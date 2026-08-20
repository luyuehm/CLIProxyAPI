// Package alerts implements multi-platform alert notifications (Feishu,
// DingTalk, and WeCom) on top of the usage accounting pipeline. It evaluates
// configurable rules against per-target usage windows and pushes alerts to the
// configured webhooks.
package alerts

import "time"

// ChannelKind identifies a notification platform.
type ChannelKind string

const (
	// ChannelFeishu targets a Feishu/Lark custom bot webhook.
	ChannelFeishu ChannelKind = "feishu"
	// ChannelDingTalk targets a DingTalk custom bot webhook.
	ChannelDingTalk ChannelKind = "dingtalk"
	// ChannelWeCom targets a WeCom (WeChat Work) group bot webhook.
	ChannelWeCom ChannelKind = "wecom"
)

// Valid reports whether the channel kind is one of the supported platforms.
func (k ChannelKind) Valid() bool {
	switch k {
	case ChannelFeishu, ChannelDingTalk, ChannelWeCom:
		return true
	default:
		return false
	}
}

// Severity classifies an alert.
type Severity string

const (
	// SeverityInfo marks informational notifications such as test messages.
	SeverityInfo Severity = "info"
	// SeverityWarning marks events that should be reviewed but are not urgent.
	SeverityWarning Severity = "warning"
	// SeverityCritical marks urgent events.
	SeverityCritical Severity = "critical"
)

// RuleKind identifies the evaluation strategy of an alert rule.
type RuleKind string

const (
	// RuleUsageLimit fires when token or request counts cross a threshold.
	RuleUsageLimit RuleKind = "usage_limit"
	// RuleAnomaly fires when the window error rate crosses a threshold.
	RuleAnomaly RuleKind = "anomaly"
	// RuleFault fires when the window error count crosses a threshold.
	RuleFault RuleKind = "fault"
)

// Valid reports whether the rule kind is supported.
func (k RuleKind) Valid() bool {
	switch k {
	case RuleUsageLimit, RuleAnomaly, RuleFault:
		return true
	default:
		return false
	}
}

// Rule is a configurable alert rule. A rule is evaluated against each
// observed usage target every check interval; the cooldown field prevents it
// from firing repeatedly for the same target within a short window.
type Rule struct {
	// Name is a stable identifier used for cooldown deduplication. Required.
	Name string `yaml:"name" json:"name"`
	// Kind selects the evaluation strategy: usage_limit, anomaly, or fault.
	Kind RuleKind `yaml:"kind" json:"kind"`
	// Severity is attached to fired events. Defaults to "warning".
	Severity Severity `yaml:"severity" json:"severity"`
	// Target scopes the rule to a specific auth-index or API key name. Empty
	// evaluates the rule against every observed target.
	Target string `yaml:"target" json:"target"`
	// TokenLimit fires a usage_limit rule when window tokens reach it.
	TokenLimit int64 `yaml:"token-limit" json:"token-limit"`
	// RequestLimit fires a usage_limit rule when window requests reach it.
	RequestLimit int64 `yaml:"request-limit" json:"request-limit"`
	// ErrorCountLimit fires a fault rule when window errors reach it.
	ErrorCountLimit int64 `yaml:"error-count-limit" json:"error-count-limit"`
	// ErrorRateLimit fires an anomaly rule when the window error rate
	// (errors / requests) reaches it. Values are in [0, 1].
	ErrorRateLimit float64 `yaml:"error-rate-limit" json:"error-rate-limit"`
	// Cooldown is the minimum interval between two fires of this rule for the
	// same target, as a Go duration string (e.g. "15m"). Defaults to the check
	// interval when empty.
	Cooldown string `yaml:"cooldown" json:"cooldown"`
}

// valid reports whether the rule is complete enough to evaluate.
func (r Rule) valid() bool {
	if r.Name == "" || !r.Kind.Valid() {
		return false
	}
	switch r.Kind {
	case RuleUsageLimit:
		return r.TokenLimit > 0 || r.RequestLimit > 0
	case RuleAnomaly:
		return r.ErrorRateLimit > 0
	case RuleFault:
		return r.ErrorCountLimit > 0
	default:
		return false
	}
}

// Event is a fired alert notification.
type Event struct {
	Time      time.Time `json:"time"`
	Rule      string    `json:"rule"`
	Kind      RuleKind  `json:"kind"`
	Severity  Severity  `json:"severity"`
	Target    string    `json:"target"`
	Message   string    `json:"message"`
	Tokens    int64     `json:"tokens,omitempty"`
	Requests  int64     `json:"requests,omitempty"`
	Errors    int64     `json:"errors,omitempty"`
	ErrorRate float64   `json:"error_rate,omitempty"`
}
