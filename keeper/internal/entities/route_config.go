package entities

import "time"

// 路由策略：固定指向一个上游，后续可扩展 failover/weight 多上游。
const (
	RouteStrategyFixed = "fixed"
)

// RouteConfig 是上游模型路由配置实体：模型 → 上游 base_url + api_key。
// Phase 2 P1 MVP 先在 Keeper 本地持久化，后续再同步到 CPA（CPA 暂无路由管理 API）。
type RouteConfig struct {
	ID          int64  `gorm:"primaryKey"`
	Model       string `gorm:"uniqueIndex:uniq_route_configs_model"`
	Enabled     bool   `gorm:"not null;default:true"`
	Strategy    string `gorm:"not null;default:fixed"`
	BaseURL     string `gorm:"not null"`
	APIKey      string
	Weight      int `gorm:"not null;default:100"`
	Description string
	CreatedAt   time.Time `gorm:"serializer:storageTime"`
	UpdatedAt   time.Time `gorm:"serializer:storageTime"`
}