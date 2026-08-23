package costallocation

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"cpa-usage-keeper/internal/entities"
	"cpa-usage-keeper/internal/helper"
	"cpa-usage-keeper/internal/timeutil"
	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

// ErrRuleNotFound 表示规则不存在。
var ErrRuleNotFound = errors.New("cost allocation rule not found")

// Service 是费用分摊模块的业务服务。
type Service struct {
	db *gorm.DB
}

func NewService(db *gorm.DB) *Service {
	return &Service{db: db}
}

// ListDepartments 返回指定时间范围内按维度聚合的部门费用列表。
// 未匹配到任何规则的 usage 归入 "Unassigned" 行。
func (s *Service) ListDepartments(from, to time.Time, dimension entities.CostAllocationDimension) (DepartmentsResponse, error) {
	if err := validateDimension(dimension); err != nil {
		return DepartmentsResponse{}, err
	}
	start, end := normalizeRange(from, to)
	rules, err := costAllocationRules(s.db, true)
	if err != nil {
		return DepartmentsResponse{}, err
	}
	rows, err := costAllocationRows(s.db, start, end)
	if err != nil {
		return DepartmentsResponse{}, err
	}

	ruleIdx := ruleIndex(rules)
	pricing, _ := loadCostPricing(s.db)

	// 按维度名+规则聚合
	type deptAccum struct {
		requests    int64
		totalTokens int64
		cost        float64
	}
	acc := make(map[string]*deptAccum)
	var unassignedCost float64
	var unassignedRequests int64

	for _, row := range rows {
		// 规则索引同时覆盖 "规则名匹配" 和 "API group key 精确匹配" 两种形态：规则名本身也作为索引键。
		key := string(dimension) + "\x00" + strings.TrimSpace(row.APIGroupKey)
		matched := ruleIdx[key]
		cost := computeRowCost(row, pricing)
		if matched != nil {
			deptName := matched.Name
			if _, ok := acc[deptName]; !ok {
				acc[deptName] = &deptAccum{}
			}
			acc[deptName].requests += row.Count
			acc[deptName].totalTokens += row.Tokens
			acc[deptName].cost += cost
		} else {
			unassignedCost += cost
			unassignedRequests += row.Count
		}
	}

	var totalCost float64
	depts := make([]DepartmentCostView, 0, len(acc))
	for name, a := range acc {
		totalCost += a.cost
		depts = append(depts, DepartmentCostView{
			Dimension:   dimension,
			Name:        name,
			Requests:    a.requests,
			TotalTokens: a.totalTokens,
			Cost:        a.cost,
			RuleCount:   1,
		})
	}
	sort.Slice(depts, func(i, j int) bool {
		return depts[i].Cost > depts[j].Cost
	})

	costAvailable := true
	for i := range depts {
		if totalCost > 0 {
			depts[i].CostShare = depts[i].Cost / totalCost * 100
		}
	}

	return DepartmentsResponse{
		Period:             fmt.Sprintf("%s - %s", start.Format("2006-01-02"), end.Format("2006-01-02")),
		Start:              start,
		End:                end,
		Departments:        depts,
		TotalCost:          totalCost,
		CostAvailable:      costAvailable,
		UnassignedCost:     unassignedCost,
		UnassignedRequests: unassignedRequests,
	}, nil
}

// ListRules 返回所有分摊规则。
func (s *Service) ListRules() ([]CostAllocationRuleView, error) {
	rules, err := costAllocationRules(s.db, false)
	if err != nil {
		return nil, err
	}
	views := make([]CostAllocationRuleView, len(rules))
	for i, rule := range rules {
		views[i] = ruleToView(rule)
	}
	return views, nil
}

// CreateRule 创建分摊规则。
func (s *Service) CreateRule(input CostAllocationRuleCreateInput) (CostAllocationRuleView, error) {
	if err := validateDimension(input.Dimension); err != nil {
		return CostAllocationRuleView{}, err
	}
	if err := validateMatchType(input.MatchType); err != nil {
		return CostAllocationRuleView{}, err
	}
	if strings.TrimSpace(input.Name) == "" {
		return CostAllocationRuleView{}, fmt.Errorf("rule name is required")
	}
	if len(input.MatchValues) == 0 {
		return CostAllocationRuleView{}, fmt.Errorf("at least one match value is required")
	}

	entity := &entities.CostAllocationRule{
		Name:        strings.TrimSpace(input.Name),
		Dimension:   input.Dimension,
		MatchType:   input.MatchType,
		MatchValues: matchValuesToEntity(input.MatchValues),
		Enabled:     input.Enabled,
		Priority:    input.Priority,
		Note:        input.Note,
	}
	saved, err := createCostAllocationRule(s.db, entity)
	if err != nil {
		return CostAllocationRuleView{}, err
	}
	return ruleToView(*saved), nil
}

// UpdateRule 更新分摊规则。仅更新非 nil 字段。
func (s *Service) UpdateRule(id int64, input CostAllocationRuleUpdateInput) (CostAllocationRuleView, error) {
	saved, err := updateCostAllocationRule(s.db, id, func(rule *entities.CostAllocationRule) error {
		if input.Name != nil {
			if strings.TrimSpace(*input.Name) == "" {
				return fmt.Errorf("rule name is required")
			}
			rule.Name = strings.TrimSpace(*input.Name)
		}
		if input.Dimension != nil {
			if err := validateDimension(*input.Dimension); err != nil {
				return err
			}
			rule.Dimension = *input.Dimension
		}
		if input.MatchType != nil {
			if err := validateMatchType(*input.MatchType); err != nil {
				return err
			}
			rule.MatchType = *input.MatchType
		}
		if input.MatchValues != nil {
			rule.MatchValues = matchValuesToEntity(*input.MatchValues)
		}
		if input.Enabled != nil {
			rule.Enabled = *input.Enabled
		}
		if input.Priority != nil {
			rule.Priority = *input.Priority
		}
		if input.Note != nil {
			rule.Note = *input.Note
		}
		return nil
	})
	if err != nil {
		return CostAllocationRuleView{}, err
	}
	return ruleToView(*saved), nil
}

// DeleteRule 删除分摊规则。
func (s *Service) DeleteRule(id int64) error {
	return deleteCostAllocationRule(s.db, id)
}

// Report 返回指定维度、时间范围的费用报表（按维度名+模型拆分）。
func (s *Service) Report(from, to time.Time, dimension entities.CostAllocationDimension) (ReportView, error) {
	if err := validateDimension(dimension); err != nil {
		return ReportView{}, err
	}
	start, end := normalizeRange(from, to)
	rules, err := costAllocationRules(s.db, true)
	if err != nil {
		return ReportView{}, err
	}
	rows, err := costAllocationRows(s.db, start, end)
	if err != nil {
		return ReportView{}, err
	}

	ruleIdx := ruleIndex(rules)
	pricing, costAvailable := loadCostPricing(s.db)

	var totalCost float64
	items := make([]ReportItem, 0)

	for _, row := range rows {
		key := string(dimension) + "\x00" + strings.TrimSpace(row.APIGroupKey)
		matched := ruleIdx[key]
		deptName := "Unassigned"
		if matched != nil {
			deptName = matched.Name
		}
		cost := computeRowCost(row, pricing)
		totalCost += cost
		items = append(items, ReportItem{
			Dimension:   dimension,
			Name:        deptName,
			Model:       row.Model,
			Requests:    row.Count,
			TotalTokens: row.Tokens,
			Cost:        cost,
		})
	}

	sort.Slice(items, func(i, j int) bool {
		if items[i].Name != items[j].Name {
			return items[i].Name < items[j].Name
		}
		return items[i].Cost > items[j].Cost
	})

	for i := range items {
		if totalCost > 0 {
			items[i].CostShare = items[i].Cost / totalCost * 100
		}
	}

	return ReportView{
		From:          start,
		To:            end,
		Dimension:     string(dimension),
		Items:         items,
		TotalCost:     totalCost,
		CostAvailable: costAvailable,
	}, nil
}

// ExportCSV 写入 CSV 格式的费用报表。
func (s *Service) ExportCSV(w io.Writer, from, to time.Time, dimension entities.CostAllocationDimension) error {
	report, err := s.Report(from, to, dimension)
	if err != nil {
		return err
	}

	writer := csv.NewWriter(w)
	defer writer.Flush()

	if err := writer.Write([]string{
		"Dimension", "Entity", "Model", "Requests", "TotalTokens", "CostUSD",
	}); err != nil {
		return fmt.Errorf("write csv header: %w", err)
	}
	for _, item := range report.Items {
		if err := writer.Write([]string{
			string(item.Dimension),
			item.Name,
			item.Model,
			fmt.Sprintf("%d", item.Requests),
			fmt.Sprintf("%d", item.TotalTokens),
			fmt.Sprintf("%.6f", item.Cost),
		}); err != nil {
			return fmt.Errorf("write csv row: %w", err)
		}
	}
	return nil
}

// normalizeRange 返回归一化的时间范围（默认 last 30 天）。
func normalizeRange(from, to time.Time) (time.Time, time.Time) {
	start := from
	if start.IsZero() {
		start = time.Now().AddDate(0, -1, 0)
	}
	end := to
	if end.IsZero() {
		end = time.Now()
	}
	return timeutil.NormalizeStorageTime(start), timeutil.NormalizeStorageTime(end)
}

func validateDimension(dimension entities.CostAllocationDimension) error {
	switch dimension {
	case entities.CostAllocationDimensionDepartment,
		entities.CostAllocationDimensionTeam,
		entities.CostAllocationDimensionProject:
		return nil
	default:
		return fmt.Errorf("unsupported dimension %q, use department, team or project", dimension)
	}
}

func validateMatchType(matchType entities.CostAllocationMatchType) error {
	switch matchType {
	case entities.CostAllocationMatchAPIKey, entities.CostAllocationMatchLabel:
		return nil
	default:
		return fmt.Errorf("unsupported match type %q, use api_key or label", matchType)
	}
}

// loadCostPricing 加载模型价格设置供费用计算使用。部分模型无定价时不影响其他模型。
func loadCostPricing(db *gorm.DB) (map[string]entities.ModelPriceSetting, bool) {
	var settings []entities.ModelPriceSetting
	if err := db.Find(&settings).Error; err != nil {
		logrus.WithError(err).Warn("cost allocation: load pricing settings failed")
		return nil, false
	}
	result := make(map[string]entities.ModelPriceSetting, len(settings))
	for _, s := range settings {
		result[strings.TrimSpace(s.Model)] = s
	}
	return result, len(result) > 0
}

func computeRowCost(row costAllocationEventRow, pricing map[string]entities.ModelPriceSetting) float64 {
	if len(pricing) == 0 {
		return 0
	}
	model := strings.TrimSpace(row.Model)
	p, hasPricing := pricing[model]
	needsPricing := helper.UsageTokenInputRequiresPricing(helper.UsageTokenCostInput{
		InputTokens:         row.Input,
		OutputTokens:        row.Output,
		CachedTokens:        row.Cached,
		CacheReadTokens:     row.CacheRead,
		CacheCreationTokens: row.CacheWrite,
	})
	if !needsPricing {
		return 0
	}
	if !hasPricing {
		return 0
	}
	return helper.CalculateUsageTokenCost(helper.UsageTokenCostInput{
		InputTokens:         row.Input,
		OutputTokens:        row.Output,
		CachedTokens:        row.Cached,
		CacheReadTokens:     row.CacheRead,
		CacheCreationTokens: row.CacheWrite,
	}, p)
}
