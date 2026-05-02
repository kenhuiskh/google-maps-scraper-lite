package gmaps

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const (
	JobStatusPending  = "pending"
	JobStatusStarting = "starting"
	JobStatusRunning  = "running"
	JobStatusPaused   = "paused"
	JobStatusBlocked  = "blocked"
	JobStatusDone     = "done"
	JobStatusFailed   = "failed"

	URLStatusPending    = "pending"
	URLStatusInProgress = "in_progress"
	URLStatusDone       = "done"
	URLStatusFailed     = "failed"
)

var (
	ErrJobPaused       = errors.New("scraper: job pause requested")
	ErrNoPendingURL    = errors.New("scraper: no pending URLs")
	ErrJobNotResumable = errors.New("scraper: job is already running or done")
	ErrActiveJobExists = errors.New("scraper: a job is already starting or running")
)

// ErrSessionBlocked is returned by Scraper.Run when consecutive place-scrape
// failures indicate the Playwright session has been rate-limited by Google.
var ErrSessionBlocked = errors.New("scraper: session blocked by Google")

// blockThreshold is the number of consecutive failures across all workers
// that triggers an ErrSessionBlocked return.
const blockThreshold = int64(10)

type JobStore struct {
	db *sql.DB
}

type Job struct {
	ID             string
	Queries        []string
	ConfigJSON     string
	Status         string
	PauseRequested bool
	CreatedAt      time.Time
	StartedAt      sql.NullTime
	UpdatedAt      time.Time
	FinishedAt     sql.NullTime
	LastError      sql.NullString
	Stats          JobStats
}

type JobStats struct {
	Total      int
	Pending    int
	InProgress int
	Done       int
	Failed     int
}

type JobTemplate struct {
	ID         string
	Name       string
	ParamsJSON string
	CreatedAt  time.Time
	LastUsedAt time.Time
}

type ClaimedURL struct {
	ID       int64
	JobID    string
	Position int
	URL      string
}

func OpenJobStore(path string) (*JobStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=5000&_journal_mode=WAL&_foreign_keys=on")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &JobStore{db: db}
	if err := s.Init(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *JobStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *JobStore) Init(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS jobs (
			id TEXT PRIMARY KEY,
			queries_json TEXT NOT NULL,
			config_json TEXT NOT NULL DEFAULT '{}',
			status TEXT NOT NULL,
			pause_requested INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL,
			started_at DATETIME,
			updated_at DATETIME NOT NULL,
			finished_at DATETIME,
			last_error TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS job_urls (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			job_id TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
			position INTEGER NOT NULL,
			url TEXT NOT NULL,
			status TEXT NOT NULL,
			attempts INTEGER NOT NULL DEFAULT 0,
			last_error TEXT,
			started_at DATETIME,
			finished_at DATETIME,
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL,
			UNIQUE(job_id, position),
			UNIQUE(job_id, url)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_job_urls_claim ON job_urls(job_id, status, position)`,
		`CREATE TABLE IF NOT EXISTS job_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			job_id TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
			event TEXT NOT NULL,
			message TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS job_templates (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			params_json TEXT NOT NULL,
			created_at DATETIME NOT NULL,
			last_used_at DATETIME NOT NULL
		)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

func (s *JobStore) CreateJob(ctx context.Context, queries []string, config any, urls []string) (string, error) {
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
			(job_id, position, url, status, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)`, id, i, u, URLStatusPending, now, now); err != nil {
			return "", err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO job_events (job_id, event, message, created_at) VALUES (?, ?, ?, ?)`,
		id, "created", fmt.Sprintf("%d URLs queued", len(urls)), now); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

func (s *JobStore) CreateStartingJob(ctx context.Context, queries []string, config any) (string, error) {
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
		(id, queries_json, config_json, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`, id, string(queriesJSON), string(configJSON), status, now, now); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO job_events (job_id, event, message, created_at) VALUES (?, ?, ?, ?)`,
		id, event, message, now); err != nil {
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
			created_at, started_at, updated_at, finished_at, last_error`,
		JobStatusStarting, now, now,
		JobStatusPending, JobStatusStarting, JobStatusRunning, JobStatusPending, JobStatusDone, JobStatusDone)
	var j Job
	var queriesJSON string
	var pause int
	if err := row.Scan(&j.ID, &queriesJSON, &j.ConfigJSON, &j.Status, &pause,
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
	_, _ = s.db.ExecContext(ctx, `INSERT INTO job_events (job_id, event, message, created_at) VALUES (?, ?, ?, ?)`,
		j.ID, "starting", "queued job claimed by scheduler", now)
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

func (s *JobStore) QueueStartingJobURLs(ctx context.Context, jobID string, urls []string) error {
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
			(job_id, position, url, status, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)`, jobID, i, u, URLStatusPending, now, now); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO job_events (job_id, event, message, created_at) VALUES (?, ?, ?, ?)`,
		jobID, "created", fmt.Sprintf("%d URLs queued", len(urls)), now); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE jobs SET updated_at = ? WHERE id = ?`, now, jobID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *JobStore) GetJob(ctx context.Context, jobID string) (*Job, error) {
	var j Job
	var queriesJSON string
	var pause int
	err := s.db.QueryRowContext(ctx, `SELECT id, queries_json, config_json, status, pause_requested,
		created_at, started_at, updated_at, finished_at, last_error
		FROM jobs WHERE id = ?`, jobID).Scan(&j.ID, &queriesJSON, &j.ConfigJSON, &j.Status, &pause,
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
	return &j, nil
}

func (s *JobStore) ListJobs(ctx context.Context) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM jobs ORDER BY created_at DESC`)
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
	jobs := make([]Job, 0, len(ids))
	for _, id := range ids {
		j, err := s.GetJob(ctx, id)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, *j)
	}
	return jobs, nil
}

func (s *JobStore) SaveJobTemplate(ctx context.Context, name string, params any) (string, error) {
	now := time.Now().UTC()
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(paramsJSON)
	id := fmt.Sprintf("tpl_%x", sum[:12])
	name = strings.TrimSpace(name)
	if name == "" {
		name = id
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO job_templates (id, name, params_json, created_at, last_used_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			params_json = excluded.params_json,
			last_used_at = excluded.last_used_at`,
		id, name, string(paramsJSON), now, now)
	if err != nil {
		return "", err
	}
	return id, nil
}

func (s *JobStore) ListJobTemplates(ctx context.Context) ([]JobTemplate, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, params_json, created_at, last_used_at
		FROM job_templates
		ORDER BY last_used_at DESC, created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var templates []JobTemplate
	for rows.Next() {
		var tpl JobTemplate
		if err := rows.Scan(&tpl.ID, &tpl.Name, &tpl.ParamsJSON, &tpl.CreatedAt, &tpl.LastUsedAt); err != nil {
			return nil, err
		}
		templates = append(templates, tpl)
	}
	return templates, rows.Err()
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
		SET status = ?, pause_requested = 0,
			started_at = COALESCE(started_at, ?), updated_at = ?,
			finished_at = NULL, last_error = NULL
		WHERE id = ? AND status NOT IN (?, ?, ?)
		RETURNING id, queries_json, config_json, status, pause_requested,
			created_at, started_at, updated_at, finished_at, last_error`,
		JobStatusRunning, now, now, jobID, JobStatusStarting, JobStatusRunning, JobStatusDone)
	var j Job
	var queriesJSON string
	var pause int
	if err := row.Scan(&j.ID, &queriesJSON, &j.ConfigJSON, &j.Status, &pause,
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
	return &j, nil
}

func (s *JobStore) ResetInProgress(ctx context.Context, jobID string) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `UPDATE job_urls SET status = ?, updated_at = ?
		WHERE job_id = ? AND status = ?`, URLStatusPending, now, jobID, URLStatusInProgress)
	return err
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
	if err := s.db.QueryRowContext(ctx, `SELECT pause_requested FROM jobs WHERE id = ?`, jobID).Scan(&pause); err != nil {
		return false, err
	}
	return pause != 0, nil
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
		RETURNING id, position, url`, URLStatusInProgress, now, now, jobID, URLStatusPending)
	var claimed ClaimedURL
	claimed.JobID = jobID
	if err := row.Scan(&claimed.ID, &claimed.Position, &claimed.URL); err != nil {
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
	return err
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

func newJobID(now time.Time) string {
	return fmt.Sprintf("job_%s_%09d", now.Format("20060102_150405"), now.Nanosecond())
}
