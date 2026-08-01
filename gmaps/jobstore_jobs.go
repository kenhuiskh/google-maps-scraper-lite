package gmaps

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

func (s *JobStore) CreateJob(ctx context.Context, queries []string, config any, urls []QueuedURL) (string, error) {
	now := time.Now().UTC()
	id := newJobID(now)
	queriesJSON, err := json.Marshal(queries)
	if err != nil {
		return "", err
	}
	configJSON, err := json.Marshal(config)
	if err != nil {
		return "", err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO jobs
		(id, queries_json, config_json, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`, id, string(queriesJSON), string(configJSON), JobStatusPending, now, now); err != nil {
		return "", err
	}
	for i, u := range urls {
		if _, err := tx.ExecContext(ctx, `INSERT INTO job_urls
			(job_id, position, url, lang, status, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, id, i, u.URL, u.Lang, URLStatusPending, now, now); err != nil {
			return "", err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO job_events (job_id, event, message, created_at) VALUES (?, ?, ?, ?)`,
		id, "created", fmt.Sprintf("%d URLs queued", len(urls)), now); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO job_execution_stats
		(job_id, queued_urls, updated_at) VALUES (?, ?, ?)`, id, len(urls), now); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

func (s *JobStore) CreateStartingJob(ctx context.Context, queries []string, config any) (string, error) {
	return s.CreateStartingJobWithSource(ctx, queries, config, "", "", "")
}

func (s *JobStore) CreateStartingJobWithSource(ctx context.Context, queries []string, config any, templateID, strategyID, strategyRunID string) (string, error) {
	now := time.Now().UTC()
	id := newJobID(now)
	queriesJSON, err := json.Marshal(queries)
	if err != nil {
		return "", err
	}
	configJSON, err := json.Marshal(config)
	if err != nil {
		return "", err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE status IN (?, ?)`,
		JobStatusStarting, JobStatusRunning).Scan(&active); err != nil {
		return "", err
	}
	status := JobStatusStarting
	event := "starting"
	message := "job accepted by control UI"
	if active > 0 {
		status = JobStatusPending
		event = "queued"
		message = "job queued by control UI"
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO jobs
		(id, queries_json, config_json, status, template_id, strategy_id, strategy_run_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), ?, ?)`,
		id, string(queriesJSON), string(configJSON), status, templateID, strategyID, strategyRunID, now, now); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO job_events (job_id, event, message, created_at) VALUES (?, ?, ?, ?)`,
		id, event, message, now); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO job_execution_stats (job_id, updated_at) VALUES (?, ?)`, id, now); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

func (s *JobStore) ClaimNextPendingJob(ctx context.Context) (*Job, error) {
	now := time.Now().UTC()
	row := s.db.QueryRowContext(ctx, `UPDATE jobs
		SET status = ?, pause_requested = 0,
			started_at = COALESCE(started_at, ?), updated_at = ?,
			finished_at = NULL, last_error = NULL
		WHERE id = (
			SELECT id
			FROM jobs
			WHERE status = ?
				AND NOT EXISTS (SELECT 1 FROM jobs WHERE status IN (?, ?))
				AND COALESCE((SELECT status FROM jobs WHERE status <> ? ORDER BY updated_at DESC, created_at DESC LIMIT 1), ?) = ?
			ORDER BY created_at ASC
			LIMIT 1
		)
		RETURNING id, queries_json, config_json, status, pause_requested,
			template_id, strategy_id, strategy_run_id,
			created_at, started_at, updated_at, finished_at, last_error`,
		JobStatusStarting, now, now,
		JobStatusPending, JobStatusStarting, JobStatusRunning, JobStatusPending, JobStatusDone, JobStatusDone)
	var j Job
	var queriesJSON string
	var pause int
	if err := row.Scan(&j.ID, &queriesJSON, &j.ConfigJSON, &j.Status, &pause,
		&j.TemplateID, &j.StrategyID, &j.StrategyRunID,
		&j.CreatedAt, &j.StartedAt, &j.UpdatedAt, &j.FinishedAt, &j.LastError); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	j.PauseRequested = pause != 0
	if err := json.Unmarshal([]byte(queriesJSON), &j.Queries); err != nil {
		return nil, err
	}
	stats, err := s.JobStats(ctx, j.ID)
	if err != nil {
		return nil, err
	}
	j.Stats = stats
	j.ExecutionStats, _ = s.JobExecutionStats(ctx, j.ID)
	_, _ = s.db.ExecContext(ctx, `INSERT INTO job_events (job_id, event, message, created_at) VALUES (?, ?, ?, ?)`,
		j.ID, "starting", "queued job claimed by scheduler", now)
	return &j, nil
}

func (s *JobStore) ClaimPendingJob(ctx context.Context, jobID string) (*Job, error) {
	now := time.Now().UTC()
	row := s.db.QueryRowContext(ctx, `UPDATE jobs
		SET status = ?, pause_requested = 0,
			started_at = COALESCE(started_at, ?), updated_at = ?,
			finished_at = NULL, last_error = NULL
		WHERE id = ?
			AND status = ?
			AND NOT EXISTS (SELECT 1 FROM jobs WHERE status IN (?, ?))
		RETURNING id, queries_json, config_json, status, pause_requested,
			template_id, strategy_id, strategy_run_id,
			created_at, started_at, updated_at, finished_at, last_error`,
		JobStatusStarting, now, now,
		jobID, JobStatusPending, JobStatusStarting, JobStatusRunning)
	var j Job
	var queriesJSON string
	var pause int
	if err := row.Scan(&j.ID, &queriesJSON, &j.ConfigJSON, &j.Status, &pause,
		&j.TemplateID, &j.StrategyID, &j.StrategyRunID,
		&j.CreatedAt, &j.StartedAt, &j.UpdatedAt, &j.FinishedAt, &j.LastError); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			status, statusErr := s.jobStatus(ctx, jobID)
			if statusErr != nil {
				return nil, statusErr
			}
			if status != JobStatusPending {
				return nil, ErrJobNotPending
			}
			return nil, ErrActiveJobExists
		}
		return nil, err
	}
	j.PauseRequested = pause != 0
	if err := json.Unmarshal([]byte(queriesJSON), &j.Queries); err != nil {
		return nil, err
	}
	stats, err := s.JobStats(ctx, j.ID)
	if err != nil {
		return nil, err
	}
	j.Stats = stats
	j.ExecutionStats, _ = s.JobExecutionStats(ctx, j.ID)
	_, _ = s.db.ExecContext(ctx, `INSERT INTO job_events (job_id, event, message, created_at) VALUES (?, ?, ?, ?)`,
		j.ID, "starting", "pending job started manually", now)
	return &j, nil
}

func (s *JobStore) NextPendingJobAfterDone(ctx context.Context) (*Job, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id
		FROM jobs
		WHERE status = ?
			AND NOT EXISTS (SELECT 1 FROM jobs WHERE status IN (?, ?))
			AND COALESCE((SELECT status FROM jobs WHERE status <> ? ORDER BY updated_at DESC, created_at DESC LIMIT 1), ?) = ?
		ORDER BY created_at ASC
		LIMIT 1`,
		JobStatusPending, JobStatusStarting, JobStatusRunning, JobStatusPending, JobStatusDone, JobStatusDone).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return s.GetJob(ctx, id)
}

func (s *JobStore) NextTimeoutPausedJob(ctx context.Context) (*Job, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT j.id
		FROM jobs j
		WHERE j.status = ?
			AND j.last_error = ?
			AND NOT EXISTS (SELECT 1 FROM jobs WHERE status IN (?, ?))
			AND (
				EXISTS (SELECT 1 FROM job_urls ju WHERE ju.job_id = j.id AND ju.status = ?)
				OR NOT EXISTS (SELECT 1 FROM job_urls ju WHERE ju.job_id = j.id)
			)
		ORDER BY j.updated_at ASC, j.created_at ASC
		LIMIT 1`,
		JobStatusPaused, JobTimeoutError, JobStatusStarting, JobStatusRunning, URLStatusPending).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return s.GetJob(ctx, id)
}

func (s *JobStore) GetJob(ctx context.Context, jobID string) (*Job, error) {
	var j Job
	var queriesJSON string
	var pause int
	err := s.db.QueryRowContext(ctx, `SELECT jobs.id, jobs.queries_json, jobs.config_json, jobs.status, jobs.pause_requested,
		jobs.template_id, jobs.strategy_id, jobs.strategy_run_id,
		job_templates.name,
		jobs.created_at, jobs.started_at, jobs.updated_at, jobs.finished_at, jobs.last_error
		FROM jobs
		LEFT JOIN job_templates ON job_templates.id = jobs.template_id
		WHERE jobs.id = ?`, jobID).Scan(&j.ID, &queriesJSON, &j.ConfigJSON, &j.Status, &pause,
		&j.TemplateID, &j.StrategyID, &j.StrategyRunID,
		&j.TemplateName,
		&j.CreatedAt, &j.StartedAt, &j.UpdatedAt, &j.FinishedAt, &j.LastError)
	if err != nil {
		return nil, err
	}
	j.PauseRequested = pause != 0
	if err := json.Unmarshal([]byte(queriesJSON), &j.Queries); err != nil {
		return nil, err
	}
	stats, err := s.JobStats(ctx, jobID)
	if err != nil {
		return nil, err
	}
	j.Stats = stats
	j.ExecutionStats, _ = s.JobExecutionStats(ctx, jobID)
	return &j, nil
}

// DeleteJob removes a job and its dependent rows (job_urls, job_events,
// job_execution_stats cascade via foreign keys). Allowed when the job is
// pending, paused, blocked, or in a terminal state (done or failed);
// otherwise returns ErrJobNotDeletable.
func (s *JobStore) DeleteJob(ctx context.Context, jobID string) error {
	var status string
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM jobs WHERE id = ?`, jobID).Scan(&status); err != nil {
		return err
	}
	if status != JobStatusPending && status != JobStatusPaused && status != JobStatusBlocked && status != JobStatusDone && status != JobStatusFailed {
		return ErrJobNotDeletable
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM jobs WHERE id = ? AND status IN (?, ?, ?, ?, ?)`,
		jobID, JobStatusPending, JobStatusPaused, JobStatusBlocked, JobStatusDone, JobStatusFailed)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrJobNotDeletable
	}
	return nil
}

// JobDeleteResult reports the outcome of deleting one job in a batch.
// Status is one of "deleted", "skipped_active", or "not_found".
type JobDeleteResult struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// BatchDeleteJobs deletes each job by reusing DeleteJob, which enforces the
// active-job guard. Active (starting/running) jobs are reported as
// "skipped_active", unknown ids as "not_found"; the rest as "deleted". A
// non-nil error is returned only for an unexpected DB failure, not for the
// per-job logical outcomes.
func (s *JobStore) BatchDeleteJobs(ctx context.Context, ids []string) ([]JobDeleteResult, error) {
	results := make([]JobDeleteResult, 0, len(ids))
	for _, id := range ids {
		status := "deleted"
		switch err := s.DeleteJob(ctx, id); {
		case err == nil:
		case errors.Is(err, ErrJobNotDeletable):
			status = "skipped_active"
		case errors.Is(err, sql.ErrNoRows):
			status = "not_found"
		default:
			return nil, err
		}
		results = append(results, JobDeleteResult{ID: id, Status: status})
	}
	return results, nil
}

// JobProcess is a recorded subprocess (and its process group) spawned for a
// job. The registry lets a new-job-start sweep reap leftover process groups
// from previous or crashed jobs (and the Chromium children they own).

func (s *JobStore) ListJobs(ctx context.Context) ([]Job, error) {
	return s.loadJobsByQuery(ctx, `SELECT id FROM jobs ORDER BY created_at DESC`)
}

// loadJobsByQuery executes the given id-selection query and returns fully
// hydrated Job records (metadata + stats + execution stats) using a constant
// number of database round-trips instead of one round-trip per job.
func (s *JobStore) loadJobsByQuery(ctx context.Context, query string, args ...any) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return s.loadJobsBatch(ctx, ids)
}

func (s *JobStore) loadJobsBatch(ctx context.Context, ids []string) ([]Job, error) {
	if len(ids) == 0 {
		return []Job{}, nil
	}
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}

	jobRows, err := s.db.QueryContext(ctx, `SELECT jobs.id, jobs.queries_json, jobs.config_json, jobs.status, jobs.pause_requested,
		jobs.template_id, jobs.strategy_id, jobs.strategy_run_id,
		job_templates.name,
		jobs.created_at, jobs.started_at, jobs.updated_at, jobs.finished_at, jobs.last_error
		FROM jobs
		LEFT JOIN job_templates ON job_templates.id = jobs.template_id
		WHERE jobs.id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]Job, len(ids))
	for jobRows.Next() {
		var j Job
		var queriesJSON string
		var pause int
		if err := jobRows.Scan(&j.ID, &queriesJSON, &j.ConfigJSON, &j.Status, &pause,
			&j.TemplateID, &j.StrategyID, &j.StrategyRunID,
			&j.TemplateName,
			&j.CreatedAt, &j.StartedAt, &j.UpdatedAt, &j.FinishedAt, &j.LastError); err != nil {
			_ = jobRows.Close()
			return nil, err
		}
		j.PauseRequested = pause != 0
		if err := json.Unmarshal([]byte(queriesJSON), &j.Queries); err != nil {
			_ = jobRows.Close()
			return nil, err
		}
		byID[j.ID] = j
	}
	if err := jobRows.Close(); err != nil {
		return nil, err
	}
	if err := jobRows.Err(); err != nil {
		return nil, err
	}

	statsRows, err := s.db.QueryContext(ctx, `SELECT job_id, status, COUNT(*) FROM job_urls WHERE job_id IN (`+placeholders+`) GROUP BY job_id, status`, args...)
	if err != nil {
		return nil, err
	}
	for statsRows.Next() {
		var jobID, status string
		var n int
		if err := statsRows.Scan(&jobID, &status, &n); err != nil {
			_ = statsRows.Close()
			return nil, err
		}
		j, ok := byID[jobID]
		if !ok {
			continue
		}
		j.Stats.Total += n
		switch status {
		case URLStatusPending:
			j.Stats.Pending = n
		case URLStatusInProgress:
			j.Stats.InProgress = n
		case URLStatusDone:
			j.Stats.Done = n
		case URLStatusFailed:
			j.Stats.Failed = n
		}
		byID[jobID] = j
	}
	if err := statsRows.Close(); err != nil {
		return nil, err
	}
	if err := statsRows.Err(); err != nil {
		return nil, err
	}

	execRows, err := s.db.QueryContext(ctx, `SELECT job_id, feed_urls_found, feed_duplicate_urls, cross_job_duplicate_urls, queued_urls,
		scraped_urls, duplicate_places, scrape_errors, write_errors, retry_events,
		watchdog_timeouts, bot_block_events, navigation_cdp_errors, page_crash_events, stall_restarts, updated_at
		FROM job_execution_stats WHERE job_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, err
	}
	for execRows.Next() {
		var es JobExecutionStats
		if err := execRows.Scan(&es.JobID, &es.FeedURLsFound, &es.FeedDuplicateURLs,
			&es.CrossJobDuplicateURLs, &es.QueuedURLs, &es.ScrapedURLs, &es.DuplicatePlaces, &es.ScrapeErrors, &es.WriteErrors,
			&es.RetryEvents, &es.WatchdogTimeouts, &es.BotBlockEvents, &es.NavigationCDPErrors, &es.PageCrashEvents,
			&es.StallRestarts, &es.UpdatedAt); err != nil {
			_ = execRows.Close()
			return nil, err
		}
		j, ok := byID[es.JobID]
		if !ok {
			continue
		}
		j.ExecutionStats = es
		byID[es.JobID] = j
	}
	if err := execRows.Close(); err != nil {
		return nil, err
	}
	if err := execRows.Err(); err != nil {
		return nil, err
	}

	out := make([]Job, 0, len(ids))
	for _, id := range ids {
		if j, ok := byID[id]; ok {
			if j.ExecutionStats.JobID == "" {
				j.ExecutionStats.JobID = id
			}
			out = append(out, j)
		}
	}
	return out, nil
}

func (s *JobStore) CountJobs(ctx context.Context) (int, error) {
	return s.CountJobsFiltered(ctx, "")
}

func (s *JobStore) CountJobsFiltered(ctx context.Context, filter string) (int, error) {
	var total int
	where, args := jobsFilterWhere(filter)
	query := `SELECT COUNT(*) FROM jobs` + where
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&total); err != nil {
		return 0, err
	}
	return total, nil
}

func (s *JobStore) ListJobsPage(ctx context.Context, limit, offset int) ([]Job, error) {
	return s.ListJobsPageFiltered(ctx, "", limit, offset)
}

func (s *JobStore) ListJobsPageFiltered(ctx context.Context, filter string, limit, offset int) ([]Job, error) {
	if limit <= 0 {
		limit = 10
	}
	if offset < 0 {
		offset = 0
	}
	where, args := jobsFilterWhere(filter)
	args = append(args, limit, offset)
	return s.loadJobsByQuery(ctx, `SELECT id FROM jobs`+where+` ORDER BY created_at DESC LIMIT ? OFFSET ?`, args...)
}

func (s *JobStore) ListJobIDsFiltered(ctx context.Context, filter string) ([]string, error) {
	where, args := jobsFilterWhere(filter)
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM jobs`+where+` ORDER BY created_at DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func jobsFilterWhere(filter string) (string, []any) {
	switch filter {
	case "pending":
		return ` WHERE status = ?`, []any{JobStatusPending}
	case "active":
		return ` WHERE status IN (?, ?)`, []any{JobStatusStarting, JobStatusRunning}
	case "done":
		return ` WHERE status = ?`, []any{JobStatusDone}
	default:
		return "", nil
	}
}

func (s *JobStore) StartJob(ctx context.Context, jobID string) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET status = ?, pause_requested = 0,
		started_at = COALESCE(started_at, ?), updated_at = ?, finished_at = NULL, last_error = NULL
		WHERE id = ?`, JobStatusRunning, now, now, jobID)
	return err
}

func (s *JobStore) ClaimResume(ctx context.Context, jobID string) (*Job, error) {
	now := time.Now().UTC()
	row := s.db.QueryRowContext(ctx, `UPDATE jobs
		SET status = CASE
				WHEN NOT EXISTS (SELECT 1 FROM job_urls WHERE job_id = jobs.id) THEN ?
				ELSE ?
			END,
			pause_requested = 0,
			started_at = COALESCE(started_at, ?), updated_at = ?,
			finished_at = NULL, last_error = NULL
		WHERE id = ? AND status NOT IN (?, ?, ?)
		RETURNING id, queries_json, config_json, status, pause_requested,
			template_id, strategy_id, strategy_run_id,
			created_at, started_at, updated_at, finished_at, last_error`,
		JobStatusStarting, JobStatusRunning, now, now, jobID, JobStatusStarting, JobStatusRunning, JobStatusDone)
	var j Job
	var queriesJSON string
	var pause int
	if err := row.Scan(&j.ID, &queriesJSON, &j.ConfigJSON, &j.Status, &pause,
		&j.TemplateID, &j.StrategyID, &j.StrategyRunID,
		&j.CreatedAt, &j.StartedAt, &j.UpdatedAt, &j.FinishedAt, &j.LastError); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrJobNotResumable
		}
		return nil, err
	}
	j.PauseRequested = pause != 0
	if err := json.Unmarshal([]byte(queriesJSON), &j.Queries); err != nil {
		return nil, err
	}
	stats, err := s.JobStats(ctx, jobID)
	if err != nil {
		return nil, err
	}
	j.Stats = stats
	j.ExecutionStats, _ = s.JobExecutionStats(ctx, jobID)
	return &j, nil
}

func (s *JobStore) ResetInProgress(ctx context.Context, jobID string) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `UPDATE job_urls SET status = ?, updated_at = ?
		WHERE job_id = ? AND status = ?`, URLStatusPending, now, jobID, URLStatusInProgress)
	return err
}

func (s *JobStore) RecoverStaleActiveJob(ctx context.Context, jobID string, staleErr error) error {
	status, err := s.jobStatus(ctx, jobID)
	if err != nil {
		return err
	}
	switch status {
	case JobStatusRunning:
		if err := s.ResetInProgress(ctx, jobID); err != nil {
			return err
		}
		if err := s.SetJobStatus(ctx, jobID, JobStatusPaused, staleErr); err != nil {
			return err
		}
	case JobStatusStarting:
		stats, err := s.JobStats(ctx, jobID)
		if err != nil {
			return err
		}
		if stats.Total > 0 {
			if err := s.ResetInProgress(ctx, jobID); err != nil {
				return err
			}
			if err := s.SetJobStatus(ctx, jobID, JobStatusPaused, staleErr); err != nil {
				return err
			}
		} else {
			// Discovery is deliberately persisted only after every feed has been
			// collected. A wall-clock timeout during that phase therefore has no
			// URLs to resume yet; preserve it as a paused job so ClaimResume can
			// put it back in starting and rerun discovery. Other startup failures
			// remain failed for operator attention.
			status := JobStatusFailed
			if staleErr != nil && staleErr.Error() == JobTimeoutError {
				status = JobStatusPaused
			}
			if err := s.SetJobStatus(ctx, jobID, status, staleErr); err != nil {
				return err
			}
		}
	default:
		return ErrJobNotStale
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE jobs SET pause_requested = 0 WHERE id = ?`, jobID)
	_, _ = s.db.ExecContext(ctx, `INSERT INTO job_events (job_id, event, message, created_at) VALUES (?, ?, ?, ?)`,
		jobID, "recovered_stale", "stale active job recovered manually", time.Now().UTC())
	return nil
}

// RecoverInterruptedJobs resets jobs left in an active state (starting/running)
// by a crashed or killed process — e.g. after a container restart. Each is
// reset via RecoverStaleActiveJob so partially-scraped work becomes resumable
// instead of permanently blocking the queue (which gates on no active job).
// Returns the number of jobs recovered.
func (s *JobStore) RecoverInterruptedJobs(ctx context.Context, reason error) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM jobs WHERE status IN (?, ?)`,
		JobStatusStarting, JobStatusRunning)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	var recovered int
	for _, id := range ids {
		if err := s.RecoverStaleActiveJob(ctx, id, reason); err != nil {
			if errors.Is(err, ErrJobNotStale) {
				continue
			}
			return recovered, err
		}
		recovered++
	}
	return recovered, nil
}

func (s *JobStore) jobStatus(ctx context.Context, jobID string) (string, error) {
	var status string
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM jobs WHERE id = ?`, jobID).Scan(&status); err != nil {
		return "", err
	}
	return status, nil
}

func (s *JobStore) RequestPause(ctx context.Context, jobID string) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET pause_requested = 1, updated_at = ? WHERE id = ?`, now, jobID)
	return err
}

func (s *JobStore) ClearPause(ctx context.Context, jobID string) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET pause_requested = 0, updated_at = ? WHERE id = ?`, now, jobID)
	return err
}

func (s *JobStore) PauseRequested(ctx context.Context, jobID string) (bool, error) {
	var pause int
	err := s.db.QueryRowContext(ctx, `SELECT pause_requested FROM jobs WHERE id = ?`, jobID).Scan(&pause)
	return pause != 0, err
}

func (s *JobStore) SetJobStatus(ctx context.Context, jobID, status string, jobErr error) error {
	now := time.Now().UTC()
	msg := sql.NullString{}
	if jobErr != nil {
		msg.Valid = true
		msg.String = jobErr.Error()
	}
	finishedStatuses := map[string]bool{JobStatusDone: true, JobStatusPaused: true, JobStatusBlocked: true, JobStatusFailed: true}
	if finishedStatuses[status] {
		_, err := s.db.ExecContext(ctx, `UPDATE jobs SET status = ?, last_error = ?, finished_at = ?,
			updated_at = ? WHERE id = ?`, status, msg, now, now, jobID)
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET status = ?, last_error = ?, updated_at = ? WHERE id = ?`,
		status, msg, now, jobID)
	return err
}
