package migration

import (
	"cpa-usage-keeper/internal/entities"

	"gorm.io/gorm"
)

func createUsersTableMigration(tx *gorm.DB) error {
	return tx.AutoMigrate(&entities.User{})
}