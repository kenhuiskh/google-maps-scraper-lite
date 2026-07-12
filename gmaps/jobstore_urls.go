package gmaps

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

func (s *JobStore) QueueStartingJobURLs(ctx context.Context, jobID string, urls []QueuedURL) error {
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM jobs WHERE id = ?`, jobID).Scan(&status); err != nil {
		return err
	}
	if status != JobStatusStarting {
		return fmt.Errorf("scraper: job %s is %s, not starting", jobID, status)
	}
	for i, u := range urls {
		if _, err := tx.ExecContext(ctx, `INSERT INTO job_urls
			(job_id, position, url, lang, status, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, jobID, i, u.URL, u.Lang, URLStatusPending, now, now); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO job_events (job_id, event, message, created_at) VALUES (?, ?, ?, ?)`,
		jobID, "created", fmt.Sprintf("%d URLs queued", len(urls)), now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO job_execution_stats
		(job_id, queued_urls, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(job_id) DO UPDATE SET queued_urls = excluded.queued_urls, updated_at = excluded.updated_at`,
		jobID, len(urls), now); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE jobs SET updated_at = ? WHERE id = ?`, now, jobID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// JobLangs returns the distinct, ordered languages present in a job's queued
// URLs. The authoritative language set comes from the job config; this is a
// consistency check / fallback for resume paths.
func (s *JobStore) JobLangs(ctx context.Context, jobID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT lang FROM job_urls WHERE job_id = ? ORDER BY lang`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var langs []string
	for rows.Next() {
		var lang string
		if err := rows.Scan(&lang); err != nil {
			return nil, err
		}
		langs = append(langs, lang)
	}
	return langs, rows.Err()
}

func (s *JobStore) ClaimNextURL(ctx context.Context, jobID string) (*ClaimedURL, error) {
	now := time.Now().UTC()
	row := s.db.QueryRowContext(ctx, `UPDATE job_urls
		SET status = ?, attempts = attempts + 1, started_at = ?, updated_at = ?, last_error = NULL
		WHERE id = (
			SELECT ju.id
			FROM job_urls ju
			JOIN jobs j ON j.id = ju.job_id
			WHERE ju.job_id = ? AND ju.status = ? AND j.pause_requested = 0
			ORDER BY ju.position
			LIMIT 1
		)
		RETURNING id, position, url, lang, attempts`, URLStatusInProgress, now, now, jobID, URLStatusPending)
	var claimed ClaimedURL
	claimed.JobID = jobID
	if err := row.Scan(&claimed.ID, &claimed.Position, &claimed.URL, &claimed.Lang, &claimed.Attempts); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			paused, pauseErr := s.PauseRequested(ctx, jobID)
			if pauseErr != nil {
				return nil, pauseErr
			}
			if paused {
				return nil, ErrJobPaused
			}
			return nil, ErrNoPendingURL
		}
		return nil, err
	}
	return &claimed, nil
}

func (s *JobStore) MarkURLDone(ctx context.Context, urlID int64) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `UPDATE job_urls SET status = ?, finished_at = ?, updated_at = ?
		WHERE id = ?`, URLStatusDone, now, now, urlID)
	return err
}

func (s *JobStore) MarkURLFailed(ctx context.Context, urlID int64, scrapeErr error) error {
	now := time.Now().UTC()
	msg := ""
	if scrapeErr != nil {
		msg = scrapeErr.Error()
	}
	_, err := s.db.ExecContext(ctx, `UPDATE job_urls SET status = ?, last_error = ?, updated_at = ?
		WHERE id = ?`, URLStatusFailed, msg, now, urlID)
	return err
}

func (s *JobStore) RequeueURL(ctx context.Context, urlID int64, scrapeErr error) error {
	now := time.Now().UTC()
	msg := ""
	if scrapeErr != nil {
		msg = scrapeErr.Error()
	}
	_, err := s.db.ExecContext(ctx, `UPDATE job_urls SET status = ?, last_error = ?, updated_at = ?
		WHERE id = ?`, URLStatusPending, msg, now, urlID)
	if err == nil {
		if jobID, lookupErr := s.jobIDForURL(ctx, urlID); lookupErr == nil {
			_ = s.IncrementJobStat(ctx, jobID, "retry_events", 1)
			_ = s.IncrementJobStat(ctx, jobID, "scrape_errors", 1)
		}
	}
	return err
}

func (s *JobStore) jobIDForURL(ctx context.Context, urlID int64) (string, error) {
	var jobID string
	err := s.db.QueryRowContext(ctx, `SELECT job_id FROM job_urls WHERE id = ?`, urlID).Scan(&jobID)
	return jobID, err
}

func (s *JobStore) FilterAlreadyScrapedURLs(ctx context.Context, urls []QueuedURL, strategyRunID string, allTime bool, firstLang string) (kept []QueuedURL, skipped int, err error) {
	if len(urls) == 0 {
		return []QueuedURL{}, 0, nil
	}
	if !allTime && strategyRunID == "" {
		return append([]QueuedURL(nil), urls...), 0, nil
	}

	key := func(url, lang string) string {
		if lang == "" {
			lang = firstLang
		}
		return url + "\x00" + lang
	}

	// Query only the candidate URLs (chunked to stay under SQLite's parameter
	// limit) so startup work scales with the current feed, not the full scrape
	// history. Matches are collected into a set keyed by (url, normalized lang),
	// then candidates are filtered in their original order.
	const chunkSize = 900
	done := make(map[string]struct{})
	for start := 0; start < len(urls); start += chunkSize {
		end := start + chunkSize
		if end > len(urls) {
			end = len(urls)
		}
		chunk := urls[start:end]

		placeholders := strings.Repeat("?,", len(chunk))
		placeholders = placeholders[:len(placeholders)-1]

		var query string
		args := make([]any, 0, len(chunk)+1)
		if allTime {
			query = `SELECT DISTINCT url, lang FROM job_urls WHERE status='done' AND url IN (` + placeholders + `)`
		} else {
			query = `SELECT DISTINCT ju.url, ju.lang FROM job_urls ju JOIN jobs j ON ju.job_id=j.id WHERE ju.status='done' AND j.strategy_run_id=? AND ju.url IN (` + placeholders + `)`
			args = append(args, strategyRunID)
		}
		for _, u := range chunk {
			args = append(args, u.URL)
		}

		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, 0, err
		}
		for rows.Next() {
			var u, lang string
			if err := rows.Scan(&u, &lang); err != nil {
				rows.Close()
				return nil, 0, err
			}
			done[key(u, lang)] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, 0, err
		}
		rows.Close()
	}

	kept = make([]QueuedURL, 0, len(urls))
	for _, u := range urls {
		if _, ok := done[key(u.URL, u.Lang)]; ok {
			skipped++
			continue
		}
		kept = append(kept, u)
	}
	return kept, skipped, nil
}
