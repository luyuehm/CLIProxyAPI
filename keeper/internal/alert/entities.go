package alert

import "time"

type Platform string

const (
	PlatformFeishu   Platform = "feishu"
	PlatformDingTalk Platform = "dingtalk"
	PlatformWeCom    Platform = "wecom"
)

func ValidPlatforms() []Platform {
	return []Platform{PlatformFeishu, PlatformDingTalk, PlatformWeCom}
}

func IsValidPlatform(s string) bool {
	for _, p := range ValidPlatforms() {
		if string(p) == s {
			return true
		}
	}
	return false
}

type MetricType string

const (
	MetricUsageThreshold MetricType = "usage_threshold"
	MetricQuotaExhausted MetricType = "quota_exhausted"
	MetricErrorRate      MetricType = "error_rate"
)

func ValidMetricTypes() []MetricType {
	return []MetricType{MetricUsageThreshold, MetricQuotaExhausted, MetricErrorRate}
}

func IsValidMetricType(s string) bool {
	for _, m := range ValidMetricTypes() {
		if string(m) == s {
			return true
		}
	}
	return false
}

type ConditionOperator string

const (
	OpGT  ConditionOperator = "gt"
	OpGTE ConditionOperator = "gte"
	OpLT  ConditionOperator = "lt"
	OpLTE ConditionOperator = "lte"
)

func ValidOperators() []ConditionOperator {
	return []ConditionOperator{OpGT, OpGTE, OpLT, OpLTE}
}

func IsValidOperator(s string) bool {
	for _, o := range ValidOperators() {
		if string(o) == s {
			return true
		}
	}
	return false
}

type AlertEventStatus string

const (
	EventStatusPending AlertEventStatus = "pending"
	EventStatusSent    AlertEventStatus = "sent"
	EventStatusFailed  AlertEventStatus = "failed"
)

// AlertChannel 告警通道配置。
type AlertChannel struct {
	ID         int64     `gorm:"primaryKey" json:"id"`
	Name       string    `gorm:"size:128;not null" json:"name"`
	Platform   string    `gorm:"size:32;not null;index" json:"platform"`
	WebhookURL string    `gorm:"size:1024;not null" json:"webhook_url"`
	Secret     string    `gorm:"size:256" json:"secret,omitempty"`
	Enabled    bool      `gorm:"not null;default:true" json:"enabled"`
	CreatedAt  time.Time `gorm:"serializer:storageTime" json:"created_at"`
	UpdatedAt  time.Time `gorm:"serializer:storageTime" json:"updated_at"`
}

func (AlertChannel) TableName() string {
	return "alert_channels"
}

// AlertRule 告警规则配置。
type AlertRule struct {
	ID           int64             `gorm:"primaryKey" json:"id"`
	Name         string            `gorm:"size:128;not null" json:"name"`
	MetricType   string            `gorm:"size:32;not null;index" json:"metric_type"`
	ConditionOp  string            `gorm:"size:8;not null" json:"condition_op"`
	ConditionVal float64           `gorm:"not null" json:"condition_val"`
	ChannelID    int64             `gorm:"not null;index" json:"channel_id"`
	Enabled      bool              `gorm:"not null;default:true" json:"enabled"`
	CreatedAt    time.Time         `gorm:"serializer:storageTime" json:"created_at"`
	UpdatedAt    time.Time         `gorm:"serializer:storageTime" json:"updated_at"`
}

func (AlertRule) TableName() string {
	return "alert_rules"
}

// AlertEvent 告警事件记录。
type AlertEvent struct {
	ID           int64            `gorm:"primaryKey" json:"id"`
	RuleID       int64            `gorm:"not null;index" json:"rule_id"`
	ChannelID    int64            `gorm:"not null;index" json:"channel_id"`
	Status       string           `gorm:"size:16;not null;default:pending;index" json:"status"`
	Message      string           `gorm:"size:2048" json:"message"`
	AttemptCount int              `gorm:"not null;default:0" json:"attempt_count"`
	LastError    string           `gorm:"size:512" json:"last_error,omitempty"`
	CreatedAt    time.Time        `gorm:"serializer:storageTime" json:"created_at"`
	UpdatedAt    time.Time        `gorm:"serializer:storageTime" json:"updated_at"`
}

func (AlertEvent) TableName() string {
	return "alert_events"
}