package repository

import (
	"fmt"
	"strings"

	"cpa-usage-keeper/internal/entities"
	"cpa-usage-keeper/internal/repository/dto"
	"gorm.io/gorm"
)

var routeConfigColumns = []string{
	"id", "model", "enabled", "strategy", "base_url",
	"api_key", "weight", "description", "created_at", "updated_at",
}

func ListRouteConfigs(db *gorm.DB) ([]dto.RouteConfigEntry, error) {
	if db == nil {
		return nil, fmt.Errorf("database is nil")
	}
	var configs []entities.RouteConfig
	if err := db.Select(routeConfigColumns).Order("model asc").Find(&configs).Error; err != nil {
		return nil, fmt.Errorf("list route configs: %w", err)
	}
	entries := make([]dto.RouteConfigEntry, 0, len(configs))
	for _, c := range configs {
		entries = append(entries, toRouteConfigEntry(c))
	}
	return entries, nil
}

func GetRouteConfig(db *gorm.DB, model string) (*dto.RouteConfigEntry, error) {
	if db == nil {
		return nil, fmt.Errorf("database is nil")
	}
	var cfg entities.RouteConfig
	if err := db.Select(routeConfigColumns).Where("model = ?", strings.TrimSpace(model)).First(&cfg).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("get route config: %w", err)
	}
	entry := toRouteConfigEntry(cfg)
	return &entry, nil
}

func UpsertRouteConfig(db *gorm.DB, input dto.RouteConfigInput) (*dto.RouteConfigEntry, error) {
	if db == nil {
		return nil, fmt.Errorf("database is nil")
	}
	modelName := strings.TrimSpace(input.Model)
	if modelName == "" {
		return nil, fmt.Errorf("model is required")
	}
	strategy := strings.TrimSpace(input.Strategy)
	if strategy == "" {
		strategy = entities.RouteStrategyFixed
	}
	baseURL := strings.TrimSpace(input.BaseURL)
	if baseURL == "" {
		return nil, fmt.Errorf("base_url is required")
	}

	cfg := &entities.RouteConfig{}
	if err := db.Select(routeConfigColumns).Where("model = ?", modelName).First(cfg).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			cfg = &entities.RouteConfig{Model: modelName}
		} else {
			return nil, fmt.Errorf("load route config: %w", err)
		}
	}

	cfg.Model = modelName
	cfg.Enabled = input.Enabled
	cfg.Strategy = strategy
	cfg.BaseURL = baseURL
	cfg.APIKey = input.APIKey
	cfg.Weight = input.Weight
	cfg.Description = input.Description

	if err := db.Save(cfg).Error; err != nil {
		return nil, fmt.Errorf("save route config: %w", err)
	}
	entry := toRouteConfigEntry(*cfg)
	return &entry, nil
}

func DeleteRouteConfig(db *gorm.DB, model string) error {
	if db == nil {
		return fmt.Errorf("database is nil")
	}
	modelName := strings.TrimSpace(model)
	if modelName == "" {
		return fmt.Errorf("model is required")
	}
	if err := db.Where("model = ?", modelName).Delete(&entities.RouteConfig{}).Error; err != nil {
		return fmt.Errorf("delete route config: %w", err)
	}
	return nil
}

func toRouteConfigEntry(c entities.RouteConfig) dto.RouteConfigEntry {
	return dto.RouteConfigEntry{
		ID:          c.ID,
		Model:       c.Model,
		Enabled:     c.Enabled,
		Strategy:    c.Strategy,
		BaseURL:     c.BaseURL,
		APIKey:      c.APIKey,
		Weight:      c.Weight,
		Description: c.Description,
		CreatedAt:   c.CreatedAt,
		UpdatedAt:   c.UpdatedAt,
	}
}