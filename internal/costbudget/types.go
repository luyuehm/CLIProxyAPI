package costbudget

import (
	"time"

	"cpa-usage-keeper/internal/entities"
)

// BudgetConfigView 是预算配置的对外视图。
type BudgetConfigView struct {
	Period         entities.BudgetPeriod `json:"period"`
	Amount         float64              `json:"amount"`
	Currency       string               `json:"currency"`
	AlertThreshold float64              `json:"alert_threshold"`
	AlertEnabled   bool                 `json:"alert_enabled"`
	AlertFired     bool                 `json:"alert_fired"`
	PeriodStart    time.Time            `json:"period_start"`
	PeriodEnd      time.Time            `json:"period_end"`
	UpdatedAt      time.Time            `json:"updated_at"`
}

// BudgetUsageView 是预算使用进度的对外视图。
type BudgetUsageView struct {
	Period         entities.BudgetPeriod `json:"period"`
	Amount         float64               `json:"amount"`
	Currency       string                `json:"currency"`
	Spent          float64               `json:"spent"`
	Remaining      float64               `json:"remaining"`
	UsagePercent   float64               `json:"usage_percent"`
	AlertThreshold float64               `json:"alert_threshold"`
	AlertEnabled   bool                  `json:"alert_enabled"`
	AlertFired     bool                  `json:"alert_fired"`
	Exceeded       bool                  `json:"exceeded"`
	PeriodStart    time.Time             `json:"period_start"`
	PeriodEnd      time.Time             `json:"period_end"`
	CostAvailable  bool                  `json:"cost_available"`
}

// BudgetReportItem 是预算报表中按模型拆分的单项。
type BudgetReportItem struct {
	Model       string  `json:"model"`
	Requests    int64   `json:"requests"`
	TotalTokens int64   `json:"total_tokens"`
	Cost        float64 `json:"cost"`
	CostShare   float64 `json:"cost_share"`
}

// BudgetReportView 是预算报表的对外视图。
type BudgetReportView struct {
	Period      entities.BudgetPeriod `json:"period"`
	Amount      float64               `json:"amount"`
	Currency    string                `json:"currency"`
	Spent       float64               `json:"spent"`
	UsagePercent float64              `json:"usage_percent"`
	PeriodStart time.Time             `json:"period_start"`
	PeriodEnd   time.Time             `json:"period_end"`
	Items       []BudgetReportItem    `json:"items"`
}

// BudgetUpdateInput 是预算配置的写入参数。
type BudgetUpdateInput struct {
	Period         entities.BudgetPeriod
	Amount         float64
	AlertThreshold float64
	AlertEnabled   bool
}
