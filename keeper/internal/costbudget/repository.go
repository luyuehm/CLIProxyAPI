package costbudget

import (
	"fmt"
	"strings"
	"time"

	"cpa-usage-keeper/internal/entities"
	"cpa-usage-keeper/internal/helper"
	"cpa-usage-keeper/internal/timeutil"
	"gorm.io/gorm"
)

// periodBudgetRow 是预算配置的仓储行。
type periodBudgetRow struct {
	Model   string  `gorm:"column:model"`
	Count   int64   `gorm:"column:request_count"`
	Tokens  int64   `gorm:"column:total_tokens"`
	Input   int64   `gorm:"column:input_tokens"`
	Output  int64   `gorm:"column:output_tokens"`
	Cached  int64   `gorm:"column:cached_tokens"`
	CacheRead int64 `gorm:"column:cache_read_tokens"`
	CacheWrite int64 `gorm:"column:cache_creation_tokens"`
}

func getBudgetConfig(db *gorm.DB, period entities.BudgetPeriod) (*entities.BudgetConfig, error) {
	if db == nil {
		return nil, fmt.Errorf("database is nil")
	}
	var config entities.BudgetConfig
	if err := db.Where("period = ?", string(period)).First(&config).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("get budget config: %w", err)
	}
	return &config, nil
}

func upsertBudgetConfig(db *gorm.DB, config *entities.BudgetConfig) (*entities.BudgetConfig, error) {
	if db == nil {
		return nil, fmt.Errorf("database is nil")
	}
	var existing entities.BudgetConfig
	if err := db.Where("period = ?", string(config.Period)).First(&existing).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			config.ID = 0
		} else {
			return nil, fmt.Errorf("load existing budget config: %w", err)
		}
	} else {
		config.ID = existing.ID
	}
	if err := db.Save(config).Error; err != nil {
		return nil, fmt.Errorf("save budget config: %w", err)
	}
	saved := *config
	return &saved, nil
}

// periodUsageCost 按模型聚合并按当前价格设置计算一个周期内的总费用。
// 与 Analysis/Request Events 使用同一计价公式，保证预算口径一致。
func periodUsageCost(db *gorm.DB, start, end time.Time) (spent float64, costAvailable bool, items []BudgetReportItem, err error) {
	if db == nil {
		return 0, false, nil, fmt.Errorf("database is nil")
	}

	var rows []periodBudgetRow
	if err := db.Model(&entities.UsageEvent{}).
		Select("model, COUNT(*) AS request_count, SUM(total_tokens) AS total_tokens, "+
			"SUM(input_tokens) AS input_tokens, SUM(output_tokens) AS output_tokens, "+
			"SUM(cached_tokens) AS cached_tokens, SUM(cache_read_tokens) AS cache_read_tokens, "+
			"SUM(cache_creation_tokens) AS cache_creation_tokens").
		Where("timestamp >= ? AND timestamp <= ?", timeutil.FormatStorageTime(start), timeutil.FormatStorageTime(end)).
		Group("model").Scan(&rows).Error; err != nil {
		return 0, false, nil, fmt.Errorf("aggregate budget usage: %w", err)
	}

	pricingByModel, err := loadBudgetPriceSettings(db)
	if err != nil {
		return 0, false, nil, fmt.Errorf("load budget price settings: %w", err)
	}

	items = make([]BudgetReportItem, 0, len(rows))
	for _, row := range rows {
		model := strings.TrimSpace(row.Model)
		pricing, hasPricing := pricingByModel[model]
		requiresPricing := helper.UsageTokenInputRequiresPricing(helper.UsageTokenCostInput{
			InputTokens:         row.Input,
			OutputTokens:        row.Output,
			CacheReadTokens:     row.CacheRead,
			CacheCreationTokens: row.CacheWrite,
		})
		cost := 0.0
		switch {
		case hasPricing:
			cost = helper.CalculateUsageTokenCostBreakdown(helper.UsageTokenCostInput{
				InputTokens:         row.Input,
				OutputTokens:        row.Output,
				CacheReadTokens:     row.CacheRead,
				CacheCreationTokens: row.CacheWrite,
			}, pricing).TotalCostUSD
			costAvailable = true
		case !requiresPricing:
			costAvailable = true
		}
		spent += cost
		items = append(items, BudgetReportItem{
			Model:       model,
			Requests:    row.Count,
			TotalTokens: row.Tokens,
			Cost:        cost,
		})
	}
	return spent, costAvailable, items, nil
}

func loadBudgetPriceSettings(db *gorm.DB) (map[string]entities.ModelPriceSetting, error) {
	var settings []entities.ModelPriceSetting
	if err := db.Find(&settings).Error; err != nil {
		return nil, fmt.Errorf("load model price settings: %w", err)
	}
	result := make(map[string]entities.ModelPriceSetting, len(settings))
	for _, setting := range settings {
		result[strings.TrimSpace(setting.Model)] = setting
	}
	return result, nil
}
