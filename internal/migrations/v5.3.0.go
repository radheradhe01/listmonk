package migrations

import (
	"log"

	"github.com/jmoiron/sqlx"
	"github.com/knadh/koanf/v2"
	"github.com/knadh/stuffbin"
)

// V5_3_0 performs the DB migrations for v.5.3.0.
// Adds per-campaign daily quota column and a table to track per-hour sent counts.
func V5_3_0(db *sqlx.DB, fs stuffbin.FileSystem, ko *koanf.Koanf, lo *log.Logger) error {
	// Add nullable daily_quota to campaigns. NULL means "unlimited".
	_, err := db.Exec(`
		ALTER TABLE campaigns ADD COLUMN IF NOT EXISTS daily_quota INT NULL;
	`)
	if err != nil {
		return err
	}

	// Create a table to track per-campaign hourly sent counts.
	// Each row represents the number of messages successfully sent for a campaign
	// during a specific UTC date and hour. The primary key ensures we can
	// atomically upsert counts for the (campaign_id, date, hour) tuple.
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS campaign_send_quota (
			campaign_id  INTEGER NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE ON UPDATE CASCADE,
			date         DATE NOT NULL,
			hour         SMALLINT NOT NULL,
			sent_count   INT NOT NULL DEFAULT 0,
			PRIMARY KEY (campaign_id, date, hour)
		);

		CREATE INDEX IF NOT EXISTS idx_campaign_send_quota_date ON campaign_send_quota(date);
		CREATE INDEX IF NOT EXISTS idx_campaign_send_quota_campaign_id ON campaign_send_quota(campaign_id);
	`)
	if err != nil {
		return err
	}

	// Nothing else to do here; application code will use this table to enforce hourly/daily limits.
	log.Println("migration v5.3.0 applied: added campaigns.daily_quota and campaign_send_quota table")
	return nil
}
