package costallocation

import (
	"fmt"
	"strings"
	"time"

	"cpa-usage-keeper/internal/entities"
	"gorm.io/gorm"
)

// costAllocationEventRow 是按维度/模型聚合的费用行。
type costAllocationEventRow struct {
	APIGroupKey string `gorm:"column:api_group_key"`
	Model       string `gorm:"column:model"`
	Count       int64  `gorm:"column:request_count"`
	Tokens      int64  `gorm:"column:total_tokens"`
	Input       int64  `gorm:"column:input_tokens"`
	Output      int64  `gorm:"column:output_tokens"`
	Cached      int64  `gorm:"column:cached_tokens"`
	CacheRead   int64  `gorm:"column:cache_read_tokens"`
	CacheWrite  int64  `gorm:"column:cache_creation_tokens"`
}

func costAllocationRules(db *gorm.DB, enabledOnly bool) ([]entities.CostAllocationRule, error) {
	if db == nil {
		return nil, fmt.Errorf("database is nil")
	}
	query := db.Order("priority DESC, id ASC")
	if enabledOnly {
		query = query.Where("enabled = ?", true)
	}
	var rules []entities.CostAllocationRule
	if err := query.Find(&rules).Error; err != nil {
		return nil, fmt.Errorf("load cost allocation rules: %w", err)
	}
	return rules, nil
}

func createCostAllocationRule(db *gorm.DB, rule *entities.CostAllocationRule) (*entities.CostAllocationRule, error) {
	if db == nil {
		return nil, fmt.Errorf("database is nil")
	}
	if err := db.Create(rule).Error; err != nil {
		return nil, fmt.Errorf("create cost allocation rule: %w", err)
	}
	saved := *rule
	return &saved, nil
}

func updateCostAllocationRule(db *gorm.DB, id int64, update func(*entities.CostAllocationRule) error) (*entities.CostAllocationRule, error) {
	if db == nil {
		return nil, fmt.Errorf("database is nil")
	}
	var rule entities.CostAllocationRule
	if err := db.First(&rule, id).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, ErrRuleNotFound
		}
		return nil, fmt.Errorf("load cost allocation rule: %w", err)
	}
	if err := update(&rule); err != nil {
		return nil, err
	}
	if err := db.Save(&rule).Error; err != nil {
		return nil, fmt.Errorf("save cost allocation rule: %w", err)
	}
	saved := rule
	return &saved, nil
}

func deleteCostAllocationRule(db *gorm.DB, id int64) error {
	if db == nil {
		return fmt.Errorf("database is nil")
	}
	result := db.Delete(&entities.CostAllocationRule{}, id)
	if result.Error != nil {
		return fmt.Errorf("delete cost allocation rule: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return ErrRuleNotFound
	}
	return nil
}

// costAllocationRows 在 [start, end] 内按 API group key + model 聚合 usage 事件费用输入。
// 与 budget 模块同一成本口径：时间边界在主叫方用 FormatStorageTime 归一化。
func costAllocationRows(db *gorm.DB, start, end time.Time) ([]costAllocationEventRow, error) {
	if db == nil {
		return nil, fmt.Errorf("database is nil")
	}
	var rows []costAllocationEventRow
	if err := db.Model(&entities.UsageEvent{}).
		Select("api_group_key, model, COUNT(*) AS request_count, SUM(total_tokens) AS total_tokens, "+
			"SUM(input_tokens) AS input_tokens, SUM(output_tokens) AS output_tokens, "+
			"SUM(cached_tokens) AS cached_tokens, SUM(cache_read_tokens) AS cache_read_tokens, "+
			"SUM(cache_creation_tokens) AS cache_creation_tokens").
		Where("timestamp >= ? AND timestamp <= ?", start, end).
		Group("api_group_key, model").Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("aggregate cost allocation usage: %w", err)
	}
	return rows, nil
}

// ruleIndex 把规则列表展开成 (维度名, 匹配值) -> 规则的索引，供分摊时 O(1) 查找。
func ruleIndex(rules []entities.CostAllocationRule) map[string]*entities.CostAllocationRule {
	index := make(map[string]*entities.CostAllocationRule, len(rules)*2)
	for indexRule := range rules {
		rule := &rules[indexRule]
		key := allocationRuleKey(string(rule.Dimension), strings.TrimSpace(rule.Name))
		index[key] = rule
		for _, raw := range splitMatchValues(rule.MatchValues) {
			value := strings.TrimSpace(raw)
			if value == "" {
				continue
			}
			index[allocationRuleKey(string(rule.Dimension), value)] = rule
		}
	}
	return index
}

// splitMatchValues 把规则的逗号/换行分隔 match_values 拆分为列表。
func splitMatchValues(matchValues string) []string {
	fields := strings.Split(matchValues, ",")
	var result []string
	for _, field := range fields {
		for _, line := range strings.Split(field, "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				result = append(result, line)
			}
		}
	}
	return result
}

// allocationRuleKey 生成规则索引键；API key 精确匹配用 api_group_key 直接命中（规则名也是匹配目标）。
func allocationRuleKey(dimension, value string) string {
	return dimension + "\x00" + value
}

// matchValuesToRuleEntity 把输入的匹配值列表转换成 entity 里逗号分隔的字符串。
func matchValuesToEntity(matchValues []string) string {
	var cleaned []string
	for _, raw := range matchValues {
		value := strings.TrimSpace(raw)
		if value != "" {
			cleaned = append(cleaned, value)
		}
	}
	return strings.Join(cleaned, ",")
}

func ruleToView(rule entities.CostAllocationRule) CostAllocationRuleView {
	return CostAllocationRuleView{
		ID:          rule.ID,
		Name:        rule.Name,
		Dimension:   rule.Dimension,
		MatchType:   rule.MatchType,
		MatchValues: splitMatchValues(rule.MatchValues),
		Enabled:     rule.Enabled,
		Priority:    rule.Priority,
		Note:        rule.Note,
		CreatedAt:   rule.CreatedAt,
		UpdatedAt:   rule.UpdatedAt,
	}
}
