package migration

import (
	"cpa-usage-keeper/internal/alert"

	"gorm.io/gorm"
)

func createAlertTablesMigration(tx *gorm.DB) error {
	return tx.AutoMigrate(&alert.AlertChannel{}, &alert.AlertRule{}, &alert.AlertEvent{})
}