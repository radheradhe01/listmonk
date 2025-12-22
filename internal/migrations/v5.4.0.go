package migrations

import (
	"log"

	"github.com/jmoiron/sqlx"
	"github.com/knadh/koanf/v2"
	"github.com/knadh/stuffbin"
)

// V5_4_0 performs the DB migrations for v.5.4.0.
// Adds per-campaign send interval column.
func V5_4_0(db *sqlx.DB, fs stuffbin.FileSystem, ko *koanf.Koanf, lo *log.Logger) error {
	// Add nullable send_interval to campaigns.
	_, err := db.Exec(`
		ALTER TABLE campaigns ADD COLUMN IF NOT EXISTS send_interval TEXT NULL;
	`)
	if err != nil {
		return err
	}

	log.Println("migration v5.4.0 applied: added campaigns.send_interval")
	return nil
}
