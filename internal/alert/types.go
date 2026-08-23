package alert

import "errors"

var (
	ErrChannelNotFound = errors.New("alert channel not found")
	ErrRuleNotFound    = errors.New("alert rule not found")
	ErrEventNotFound   = errors.New("alert event not found")
)

// ChannelCreateRequest 创建告警通道请求。
type ChannelCreateRequest struct {
	Name       string `json:"name" binding:"required"`
	Platform   string `json:"platform" binding:"required"`
	WebhookURL string `json:"webhook_url" binding:"required"`
	Secret     string `json:"secret,omitempty"`
	Enabled    *bool  `json:"enabled,omitempty"`
}

// ChannelUpdateRequest 更新告警通道请求。
type ChannelUpdateRequest struct {
	Name       *string `json:"name,omitempty"`
	Platform   *string `json:"platform,omitempty"`
	WebhookURL *string `json:"webhook_url,omitempty"`
	Secret     *string `json:"secret,omitempty"`
	Enabled    *bool   `json:"enabled,omitempty"`
}

// RuleCreateRequest 创建告警规则请求。
type RuleCreateRequest struct {
	Name         string  `json:"name" binding:"required"`
	MetricType   string  `json:"metric_type" binding:"required"`
	ConditionOp  string  `json:"condition_op" binding:"required"`
	ConditionVal float64 `json:"condition_val" binding:"required"`
	ChannelID    int64   `json:"channel_id" binding:"required"`
	Enabled      *bool   `json:"enabled,omitempty"`
}

// RuleUpdateRequest 更新告警规则请求。
type RuleUpdateRequest struct {
	Name         *string  `json:"name,omitempty"`
	MetricType   *string  `json:"metric_type,omitempty"`
	ConditionOp  *string  `json:"condition_op,omitempty"`
	ConditionVal *float64 `json:"condition_val,omitempty"`
	ChannelID    *int64   `json:"channel_id,omitempty"`
	Enabled      *bool    `json:"enabled,omitempty"`
}