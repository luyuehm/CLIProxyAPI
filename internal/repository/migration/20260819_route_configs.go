package migration

import "gorm.io/gorm"

func addRouteConfigsMigration(db *gorm.DB) error {
	return db.Exec(`
		CREATE TABLE IF NOT EXISTS route_configs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			model TEXT NOT NULL UNIQUE,
			enabled INTEGER NOT NULL DEFAULT 1,
			strategy TEXT NOT NULL DEFAULT 'fixed',
			base_url TEXT NOT NULL,
			api_key TEXT DEFAULT '',
			weight INTEGER NOT NULL DEFAULT 100,
			description TEXT DEFAULT '',
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL
		)
	`).Error
}