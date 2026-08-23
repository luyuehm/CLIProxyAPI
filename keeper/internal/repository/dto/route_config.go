package dto

import "time"

// RouteConfigInput 是路由配置写入参数。
type RouteConfigInput struct {
	Model       string
	Enabled     bool
	Strategy    string
	BaseURL     string
	APIKey      string
	Weight      int
	Description string
}

// RouteConfigEntry 是路由配置查询返回条目。
type RouteConfigEntry struct {
	ID          int64
	Model       string
	Enabled     bool
	Strategy    string
	BaseURL     string
	APIKey      string
	Weight      int
	Description string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}