package entities

import "time"

// CostAllocationDimension 表示费用分摊的目标维度。
type CostAllocationDimension string

const (
	CostAllocationDimensionDepartment CostAllocationDimension = "department"
	CostAllocationDimensionTeam       CostAllocationDimension = "team"
	CostAllocationDimensionProject    CostAllocationDimension = "project"
)

// CostAllocationMatchType 表示分摊规则的匹配方式。
// api_key 按 API Key（api_group_key）精确匹配；label 按 usage identity 标签匹配。
type CostAllocationMatchType string

const (
	CostAllocationMatchAPIKey CostAllocationMatchType = "api_key"
	CostAllocationMatchLabel  CostAllocationMatchType = "label"
)

// CostAllocationRule 把若干 API Key 或标签映射到一个分摊单元（部门/团队/项目）。
type CostAllocationRule struct {
	ID          int64                   `gorm:"primaryKey;autoIncrement" json:"id"`
	Name        string                  `gorm:"size:128;not null" json:"name"`
	Dimension   CostAllocationDimension `gorm:"size:20;not null;default:department" json:"dimension"`
	MatchType   CostAllocationMatchType `gorm:"size:20;not null;default:api_key" json:"match_type"`
	MatchValues string                  `gorm:"type:text" json:"match_values"` // 逗号/换行分隔的匹配值列表
	Enabled     bool                    `gorm:"not null;default:true" json:"enabled"`
	Priority    int                     `gorm:"not null;default:0" json:"priority"`
	Note        string                  `gorm:"size:255" json:"note"`
	CreatedAt   time.Time               `gorm:"serializer:storageTime" json:"created_at"`
	UpdatedAt   time.Time               `gorm:"serializer:storageTime" json:"updated_at"`
}
