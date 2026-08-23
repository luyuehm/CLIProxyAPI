package service

import (
	"context"
	"fmt"
	"strings"

	"cpa-usage-keeper/internal/repository"
	repodto "cpa-usage-keeper/internal/repository/dto"
	"gorm.io/gorm"
)

// RouteConfigProvider 是路由配置管理的服务接口。
type RouteConfigProvider interface {
	ListRoutes(context.Context) ([]repodto.RouteConfigEntry, error)
	GetRoute(ctx context.Context, model string) (*repodto.RouteConfigEntry, error)
	UpsertRoute(ctx context.Context, input repodto.RouteConfigInput) (*repodto.RouteConfigEntry, error)
	DeleteRoute(ctx context.Context, model string) error
}

type routeConfigService struct {
	db *gorm.DB
}

func NewRouteConfigService(db *gorm.DB) RouteConfigProvider {
	return &routeConfigService{db: db}
}

func (s *routeConfigService) ListRoutes(_ context.Context) ([]repodto.RouteConfigEntry, error) {
	return repository.ListRouteConfigs(s.db)
}

func (s *routeConfigService) GetRoute(_ context.Context, model string) (*repodto.RouteConfigEntry, error) {
	return repository.GetRouteConfig(s.db, model)
}

func (s *routeConfigService) UpsertRoute(_ context.Context, input repodto.RouteConfigInput) (*repodto.RouteConfigEntry, error) {
	modelName := strings.TrimSpace(input.Model)
	if modelName == "" {
		return nil, fmt.Errorf("model is required")
	}
	baseURL := strings.TrimSpace(input.BaseURL)
	if baseURL == "" {
		return nil, fmt.Errorf("base_url is required")
	}
	strategy := strings.TrimSpace(input.Strategy)
	if strategy == "" {
		input.Strategy = "fixed"
	}
	if input.Weight < 0 || input.Weight > 100 {
		return nil, fmt.Errorf("weight must be between 0 and 100")
	}
	return repository.UpsertRouteConfig(s.db, input)
}

func (s *routeConfigService) DeleteRoute(_ context.Context, model string) error {
	return repository.DeleteRouteConfig(s.db, model)
}