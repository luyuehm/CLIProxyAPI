package costallocation

import (
	"time"

	"cpa-usage-keeper/internal/entities"
)

// DepartmentCostView 是部门/团队/项目维度的费用统计视图。
type DepartmentCostView struct {
	Dimension   entities.CostAllocationDimension `json:"dimension"`
	Name        string                           `json:"name"`
	Requests    int64                            `json:"requests"`
	TotalTokens int64                            `json:"total_tokens"`
	Cost        float64                          `json:"cost"`
	CostShare   float64                          `json:"cost_share"`
	RuleCount   int                              `json:"rule_count"`
}

// DepartmentsResponse 是部门费用列表的响应。
type DepartmentsResponse struct {
	Period             string               `json:"period"`
	Start              time.Time            `json:"start"`
	End                time.Time            `json:"end"`
	Departments        []DepartmentCostView `json:"departments"`
	TotalCost          float64              `json:"total_cost"`
	CostAvailable      bool                 `json:"cost_available"`
	UnassignedCost     float64              `json:"unassigned_cost"`
	UnassignedRequests int64                `json:"unassigned_requests"`
}

// CostAllocationRuleView 是分摊规则视图。
type CostAllocationRuleView struct {
	ID          int64                            `json:"id"`
	Name        string                           `json:"name"`
	Dimension   entities.CostAllocationDimension `json:"dimension"`
	MatchType   entities.CostAllocationMatchType `json:"match_type"`
	MatchValues []string                         `json:"match_values"`
	Enabled     bool                             `json:"enabled"`
	Priority    int                              `json:"priority"`
	Note        string                           `json:"note"`
	CreatedAt   time.Time                        `json:"created_at"`
	UpdatedAt   time.Time                        `json:"updated_at"`
}

// CostAllocationRuleCreateInput 是创建规则参数。
type CostAllocationRuleCreateInput struct {
	Name        string
	Dimension   entities.CostAllocationDimension
	MatchType   entities.CostAllocationMatchType
	MatchValues []string
	Enabled     bool
	Priority    int
	Note        string
}

// CostAllocationRuleUpdateInput 是更新规则参数。
type CostAllocationRuleUpdateInput struct {
	Name        *string
	Dimension   *entities.CostAllocationDimension
	MatchType   *entities.CostAllocationMatchType
	MatchValues *[]string
	Enabled     *bool
	Priority    *int
	Note        *string
}

// ReportItem 是费用报表单项。
type ReportItem struct {
	Dimension   entities.CostAllocationDimension `json:"dimension"`
	Name        string                           `json:"name"`
	Model       string                           `json:"model"`
	Requests    int64                            `json:"requests"`
	TotalTokens int64                            `json:"total_tokens"`
	Cost        float64                          `json:"cost"`
	CostShare   float64                          `json:"cost_share"`
}

// ReportView 是费用报表视图。
type ReportView struct {
	From          time.Time    `json:"from"`
	To            time.Time    `json:"to"`
	Dimension     string       `json:"dimension"`
	Items         []ReportItem `json:"items"`
	TotalCost     float64      `json:"total_cost"`
	CostAvailable bool         `json:"cost_available"`
}
