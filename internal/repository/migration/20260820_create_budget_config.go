package migration

import (
	"cpa-usage-keeper/internal/entities"

	"gorm.io/gorm"
)

func createBudgetConfigMigration(tx *gorm.DB) error {
	return tx.AutoMigrate(&entities.BudgetConfig{})
}
