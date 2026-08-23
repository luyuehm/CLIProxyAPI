package entities

import "cpa-usage-keeper/internal/alert"

// All 返回需要 AutoMigrate 的核心数据库实体列表。
func All() []any {
	return []any{
		&UsageEvent{},
		&RedisUsageInbox{},
		&ModelPriceSetting{},
		&UsageIdentity{},
		&CPAAPIKey{},
		&RouteConfig{},
		&UsageOverviewHourlyStat{},
		&UsageOverviewDailyStat{},
		&UsageOverviewHealthStat{},
		&UsageOverviewAggregationCheckpoint{},
		&User{},
		&ContentFilterRule{},
		&ContentFilterLog{},
		&BudgetConfig{},
		&CostAllocationRule{},
		&alert.AlertChannel{},
		&alert.AlertRule{},
		&alert.AlertEvent{},
	}
}
