package migration

import (
	"fmt"

	"cpa-usage-keeper/internal/entities"

	"gorm.io/gorm"
)

func addUsageEventStatusCodeMigration(tx *gorm.DB) error {
	if !tx.Migrator().HasTable(&entities.UsageEvent{}) {
		return nil
	}
	if tx.Migrator().HasColumn(&entities.UsageEvent{}, "status_code") {
		return nil
	}
	if err := tx.Exec("ALTER TABLE usage_events ADD COLUMN status_code INTEGER NOT NULL DEFAULT 0").Error; err != nil {
		return fmt.Errorf("add usage_events.status_code column: %w", err)
	}
	if err := tx.Exec("CREATE INDEX IF NOT EXISTS idx_usage_events_status_code ON usage_events(status_code)").Error; err != nil {
		return fmt.Errorf("add usage_events.status_code index: %w", err)
	}
	return nil
}