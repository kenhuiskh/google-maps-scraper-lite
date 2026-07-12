package gmaps

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const schedulerPausedKey = "scheduler_paused"

// SchedulerPaused returns true when the job queue scheduler has been paused
// via the control UI. Defaults to false when no setting row exists.
func (s *JobStore) SchedulerPaused(ctx context.Context) (bool, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM app_settings WHERE key = ?`, schedulerPausedKey).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return value == "1", nil
}

// SetSchedulerPaused persists the scheduler pause flag. When true, the queue
// loop skips claiming new pending jobs; jobs already running continue.
func (s *JobStore) SetSchedulerPaused(ctx context.Context, paused bool) error {
	value := "0"
	if paused {
		value = "1"
	}
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `INSERT INTO app_settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		schedulerPausedKey, value, now)
	return err
}
