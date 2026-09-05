package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"cpa-usage-keeper/internal/auth"
	"cpa-usage-keeper/internal/entities"
	"gorm.io/gorm"
)

const (
	// mfaSettingKey 保存管理员 TOTP 双因子认证配置（Base32 密钥 + 启用状态）。
	mfaSettingKey = "admin_totp_config"
)

// MFASecretStore 通过 app_settings 表持久化 TOTP 配置。
type MFASecretStore struct {
	db *gorm.DB
}

func NewMFASecretStore(db *gorm.DB) *MFASecretStore {
	return &MFASecretStore{db: db}
}

// GetTOTPConfig 读取持久化 TOTP 配置；从未设置过时返回零值配置。
func (s *MFASecretStore) GetTOTPConfig() (auth.TOTPConfig, error) {
	if s == nil || s.db == nil {
		return auth.TOTPConfig{}, fmt.Errorf("mfa store is not configured")
	}
	setting, found, err := GetAppSetting(context.Background(), s.db, mfaSettingKey)
	if err != nil {
		return auth.TOTPConfig{}, err
	}
	if !found || setting.Value == nil || *setting.Value == "" {
		return auth.TOTPConfig{}, gorm.ErrRecordNotFound
	}
	var config auth.TOTPConfig
	if err := json.Unmarshal([]byte(*setting.Value), &config); err != nil {
		return auth.TOTPConfig{}, fmt.Errorf("decode totp config: %w", err)
	}
	return config, nil
}

// SaveTOTPConfig 持久化 TOTP 配置，仅在显式校验成功后由 handler 调用。
func (s *MFASecretStore) SaveTOTPConfig(config auth.TOTPConfig) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("mfa store is not configured")
	}
	if config.Secret == "" {
		return fmt.Errorf("totp secret is required")
	}
	raw, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("encode totp config: %w", err)
	}
	value := string(raw)
	now := time.Now()
	_, err = UpsertAppSetting(context.Background(), s.db, entities.AppSetting{
		SettingKey: mfaSettingKey,
		Value:      &value,
		ValueType:  entities.AppSettingValueTypeJSON,
		CreatedAt:  now,
		UpdatedAt:  now,
	})
	return err
}