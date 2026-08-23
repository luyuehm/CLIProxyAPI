package entities

import "time"

// ContentFilterRule 对应内容过滤与 PII 脱敏规则。
type ContentFilterRule struct {
	ID             int64     `gorm:"primaryKey;autoIncrement" json:"id"`
	Name           string    `gorm:"size:128;not null" json:"name"`
	Description    string    `gorm:"size:255" json:"description"`
	Enabled        bool      `gorm:"not null;default:true" json:"enabled"`
	Scenario       string    `gorm:"size:32;not null;default:general" json:"scenario"` // general, finance, medical, custom
	Action         string    `gorm:"size:20;not null;default:mask" json:"action"`       // mask, redact, block
	SensitiveWords string    `gorm:"type:text" json:"sensitive_words"`                  // 换行或逗号分隔的敏感词列表
	PIITypes       string    `gorm:"type:text" json:"pii_types"`                        // 逗号分隔的 PII 类型列表 (phone,id_card,email,bank_card,medical_record,passport)
	WhiteList      string    `gorm:"type:text" json:"white_list"`                       // 白名单关键词/用户
	Models         string    `gorm:"type:text" json:"models"`                           // 匹配模型，支持通配符如 * 或 gpt-*
	Priority       int       `gorm:"not null;default:0" json:"priority"`
	CreatedAt      time.Time `gorm:"serializer:storageTime" json:"created_at"`
	UpdatedAt      time.Time `gorm:"serializer:storageTime" json:"updated_at"`
}

// ContentFilterLog 对应内容过滤审计与命中日志。
type ContentFilterLog struct {
	ID              int64     `gorm:"primaryKey;autoIncrement" json:"id"`
	RuleID          int64     `gorm:"index" json:"rule_id"`
	RuleName        string    `gorm:"size:128" json:"rule_name"`
	FilterType      string    `gorm:"size:32" json:"filter_type"` // sensitive_word, pii, combined
	MatchCount      int       `gorm:"not null;default:0" json:"match_count"`
	Matches         string    `gorm:"type:text" json:"matches"`  // JSON 格式或描述命中项
	Action          string    `gorm:"size:20;not null" json:"action"` // mask, redact, block
	Model           string    `gorm:"size:64" json:"model"`
	ClientIP        string    `gorm:"size:64" json:"client_ip"`
	UserID          string    `gorm:"size:64" json:"user_id"`
	RawPreview      string    `gorm:"type:text" json:"raw_preview"`      // 部分脱敏的原文本预览
	FilteredPreview string    `gorm:"type:text" json:"filtered_preview"` // 过滤/脱敏后的文本预览
	CreatedAt       time.Time `gorm:"serializer:storageTime;index" json:"created_at"`
}
