package gmaps

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

func (s *JobStore) JobStats(ctx context.Context, jobID string) (JobStats, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM job_urls WHERE job_id = ? GROUP BY status`, jobID)
	if err != nil {
		return JobStats{}, err
	}
	defer rows.Close()
	var stats JobStats
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return JobStats{}, err
		}
		stats.Total += n
		switch status {
		case URLStatusPending:
			stats.Pending = n
		case URLStatusInProgress:
			stats.InProgress = n
		case URLStatusDone:
			stats.Done = n
		case URLStatusFailed:
			stats.Failed = n
		}
	}
	return stats, rows.Err()
}

// FilterAlreadyScrapedURLs removes (url, lang) pairs already marked done in
// prior job rows. Matching is per-(url, lang): the same place URL in a not-yet
// scraped language is kept. Legacy done rows predate the lang column and carry
// lang=”; they are treated as firstLang so pre-existing single-language data is
// still recognized. Run-scoped filtering needs a strategy run id; without one it
// is a no-op so ad-hoc CLI jobs keep their historical behavior unless all-time
// mode is used.

func (s *JobStore) SetJobDiscoveryStats(ctx context.Context, jobID string, feedURLsFound, feedDuplicateURLs, crossJobDuplicateURLs, queuedURLs int) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `INSERT INTO job_execution_stats
		(job_id, feed_urls_found, feed_duplicate_urls, cross_job_duplicate_urls, queued_urls, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(job_id) DO UPDATE SET
			feed_urls_found = excluded.feed_urls_found,
			feed_duplicate_urls = excluded.feed_duplicate_urls,
			cross_job_duplicate_urls = excluded.cross_job_duplicate_urls,
			queued_urls = excluded.queued_urls,
			updated_at = excluded.updated_at`,
		jobID, feedURLsFound, feedDuplicateURLs, crossJobDuplicateURLs, queuedURLs, now)
	return err
}

func (s *JobStore) IncrementJobStat(ctx context.Context, jobID, field string, delta int) error {
	if delta == 0 {
		return nil
	}
	switch field {
	case "scraped_urls", "duplicate_places", "scrape_errors", "write_errors", "retry_events":
	default:
		return fmt.Errorf("unknown job stat field %q", field)
	}
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, fmt.Sprintf(`INSERT INTO job_execution_stats
		(job_id, %s, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(job_id) DO UPDATE SET %s = %s + excluded.%s, updated_at = excluded.updated_at`,
		field, field, field, field), jobID, delta, now)
	return err
}

func (s *JobStore) JobExecutionStats(ctx context.Context, jobID string) (JobExecutionStats, error) {
	var stats JobExecutionStats
	err := s.db.QueryRowContext(ctx, `SELECT job_id, feed_urls_found, feed_duplicate_urls, cross_job_duplicate_urls, queued_urls,
		scraped_urls, duplicate_places, scrape_errors, write_errors, retry_events, updated_at
		FROM job_execution_stats WHERE job_id = ?`, jobID).Scan(&stats.JobID, &stats.FeedURLsFound, &stats.FeedDuplicateURLs,
		&stats.CrossJobDuplicateURLs, &stats.QueuedURLs, &stats.ScrapedURLs, &stats.DuplicatePlaces, &stats.ScrapeErrors, &stats.WriteErrors,
		&stats.RetryEvents, &stats.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		stats.JobID = jobID
		return stats, nil
	}
	return stats, err
}
