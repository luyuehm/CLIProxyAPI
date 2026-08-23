package entities

import "time"

// BudgetPeriod 表示预算周期类型。
type BudgetPeriod string

const (
	BudgetPeriodMonthly  BudgetPeriod = "monthly"
	BudgetPeriodQuarterly BudgetPeriod = "quarterly"
	BudgetPeriodYearly   BudgetPeriod = "yearly"
)

// BudgetConfig 是预算配置实体，每个周期只有一条活跃配置。
type BudgetConfig struct {
	ID             int64        `gorm:"primaryKey"`
	Period         BudgetPeriod `gorm:"not null;default:monthly;uniqueIndex:uniq_budget_configs_period"`
	Amount         float64      `gorm:"not null"`
	AlertThreshold float64      `gorm:"not null;default:80"`
	AlertEnabled   bool         `gorm:"not null;default:true"`
	AlertFired     bool         `gorm:"not null;default:false"`
	PeriodStart    time.Time    `gorm:"serializer:storageTime;not null"`
	PeriodEnd      time.Time    `gorm:"serializer:storageTime;not null"`
	CreatedAt      time.Time    `gorm:"serializer:storageTime"`
	UpdatedAt      time.Time    `gorm:"serializer:storageTime"`
}
