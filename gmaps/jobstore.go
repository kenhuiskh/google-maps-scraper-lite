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

	JobTimeoutError = "exceeded job timeout"

	URLStatusPending    = "pending"
	URLStatusInProgress = "in_progress"
	URLStatusDone       = "done"
	URLStatusFailed     = "failed"
)

var (
	ErrJobPaused          = errors.New("scraper: job pause requested")
	ErrNoPendingURL       = errors.New("scraper: no pending URLs")
	ErrJobNotResumable    = errors.New("scraper: job is already running or done")
	ErrActiveJobExists    = errors.New("scraper: a job is already starting or running")
	ErrJobNotPending      = errors.New("scraper: job is not pending")
	ErrJobNotStale        = errors.New("scraper: job is not starting or running")
	ErrJobNotDeletable    = errors.New("scraper: job must be pending, paused, blocked, done, or failed to delete")
	ErrTemplateReferenced = errors.New("scraper: template is referenced by a strategy")

	ErrConfigExportInvalid  = errors.New("config export: invalid selection")
	ErrConfigImportInvalid  = errors.New("config import: invalid payload")
	ErrConfigImportConflict = errors.New("config import: conflict")
)

// ErrSessionBlocked is returned by Scraper.Run when consecutive place-scrape
// failures indicate the Playwright session has been rate-limited by Google.
var ErrSessionBlocked = errors.New("scraper: session blocked by Google")

// ErrBotBlocked is returned when Google serves a captcha/consent/sorry wall
// or a 429, indicating the session needs the long recovery pause.
var ErrBotBlocked = errors.New("scraper: bot block detected by Google")

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
	TemplateID     sql.NullString
	StrategyID     sql.NullString
	StrategyRunID  sql.NullString
	CreatedAt      time.Time
	StartedAt      sql.NullTime
	UpdatedAt      time.Time
	FinishedAt     sql.NullTime
	LastError      sql.NullString
	TemplateName   sql.NullString
	Stats          JobStats
	ExecutionStats JobExecutionStats
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

type Strategy struct {
	ID         string
	Name       string
	Notes      string
	Templates  []JobTemplate
	CreatedAt  time.Time
	UpdatedAt  time.Time
	LastUsedAt sql.NullTime
}

type JobExecutionStats struct {
	JobID                 string
	FeedURLsFound         int
	FeedDuplicateURLs     int
	CrossJobDuplicateURLs int
	QueuedURLs            int
	ScrapedURLs           int
	DuplicatePlaces       int
	ScrapeErrors          int
	WriteErrors           int
	RetryEvents           int
	UpdatedAt             sql.NullTime
}

type ClaimedURL struct {
	ID       int64
	JobID    string
	Position int
	URL      string
	Lang     string
}

// QueuedURL is a canonical place URL paired with the language it should be
// scraped in. Multi-language jobs expand each canonical URL into one QueuedURL
// per language; single-language jobs use Lang = "".
type QueuedURL struct {
	URL  string
	Lang string
}

// URLsNoLang wraps plain URLs as QueuedURL values with no language set. It is a
// convenience for callers that do not fan out by language.
func URLsNoLang(urls []string) []QueuedURL {
	out := make([]QueuedURL, len(urls))
	for i, u := range urls {
		out[i] = QueuedURL{URL: u}
	}
	return out
}

type StrategyJobCreate struct {
	Queries       []string
	Config        any
	TemplateID    string
	StrategyID    string
	StrategyRunID string
}

const ConfigExportVersion = 1

type ReusableConfigExport struct {
	Version    int                      `json:"version"`
	ExportedAt time.Time                `json:"exported_at"`
	Source     string                   `json:"source"`
	Templates  []ReusableConfigTemplate `json:"templates"`
	Strategies []ReusableConfigStrategy `json:"strategies"`
}

type ReusableConfigTemplate struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	ParamsJSON string    `json:"params_json"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at"`
}

type ReusableConfigStrategy struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Notes       string    `json:"notes"`
	TemplateIDs []string  `json:"template_ids"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	LastUsedAt  time.Time `json:"last_used_at,omitempty"`
}

type ConfigImportMode string

const (
	ConfigImportRename    ConfigImportMode = "rename"
	ConfigImportSkip      ConfigImportMode = "skip"
	ConfigImportOverwrite ConfigImportMode = "overwrite"
	ConfigImportDuplicate ConfigImportMode = "duplicate"
)

type ConfigImportSummary struct {
	Mode       ConfigImportMode          `json:"mode"`
	Templates  ConfigImportEntitySummary `json:"templates"`
	Strategies ConfigImportEntitySummary `json:"strategies"`
	Warnings   []string                  `json:"warnings,omitempty"`
}

type ConfigImportEntitySummary struct {
	Created    int `json:"created"`
	Updated    int `json:"updated"`
	Skipped    int `json:"skipped"`
	Renamed    int `json:"renamed"`
	Duplicated int `json:"duplicated"`
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
			lang TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL,
			attempts INTEGER NOT NULL DEFAULT 0,
			last_error TEXT,
			started_at DATETIME,
			finished_at DATETIME,
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL,
			UNIQUE(job_id, position),
			UNIQUE(job_id, url, lang)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_job_urls_claim ON job_urls(job_id, status, position)`,
		`CREATE INDEX IF NOT EXISTS idx_job_urls_url_status ON job_urls(url, status)`,
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
	return s.migrate(ctx)
}

func (s *JobStore) migrate(ctx context.Context) error {
	if err := s.ensureColumn(ctx, "jobs", "template_id", "TEXT REFERENCES job_templates(id) ON DELETE SET NULL"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "jobs", "strategy_id", "TEXT"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "jobs", "strategy_run_id", "TEXT"); err != nil {
		return err
	}
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS strategies (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			notes TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL,
			last_used_at DATETIME
		)`,
		`CREATE TABLE IF NOT EXISTS strategy_templates (
			strategy_id TEXT NOT NULL REFERENCES strategies(id) ON DELETE CASCADE,
			template_id TEXT NOT NULL REFERENCES job_templates(id) ON DELETE RESTRICT,
			position INTEGER NOT NULL,
			created_at DATETIME NOT NULL,
			PRIMARY KEY(strategy_id, template_id),
			UNIQUE(strategy_id, position)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_strategy_templates_position ON strategy_templates(strategy_id, position)`,
		`CREATE TABLE IF NOT EXISTS app_settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			updated_at DATETIME NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS job_execution_stats (
			job_id TEXT PRIMARY KEY REFERENCES jobs(id) ON DELETE CASCADE,
			feed_urls_found INTEGER NOT NULL DEFAULT 0,
			feed_duplicate_urls INTEGER NOT NULL DEFAULT 0,
			cross_job_duplicate_urls INTEGER NOT NULL DEFAULT 0,
			queued_urls INTEGER NOT NULL DEFAULT 0,
			scraped_urls INTEGER NOT NULL DEFAULT 0,
			duplicate_places INTEGER NOT NULL DEFAULT 0,
			scrape_errors INTEGER NOT NULL DEFAULT 0,
			write_errors INTEGER NOT NULL DEFAULT 0,
			retry_events INTEGER NOT NULL DEFAULT 0,
			updated_at DATETIME NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS job_processes (
			job_id TEXT PRIMARY KEY,
			pid INTEGER NOT NULL,
			pgid INTEGER NOT NULL,
			started_at DATETIME NOT NULL
		)`,
		`PRAGMA user_version = 1`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	if err := s.ensureColumn(ctx, "job_execution_stats", "cross_job_duplicate_urls", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.migrateJobURLsLang(ctx); err != nil {
		return err
	}
	return nil
}

// hasColumn reports whether table has a column with the given name.
func (s *JobStore) hasColumn(ctx context.Context, table, column string) (bool, error) {
	if err := validateSQLIdentifier(table); err != nil {
		return false, err
	}
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid          int
			name, typ    string
			notNull, pk  int
			defaultValue any
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// migrateJobURLsLang adds the job_urls.lang column and changes the row-identity
// uniqueness from (job_id, url) to (job_id, url, lang) so the same canonical
// place URL can be queued once per language. SQLite cannot alter an inline
// UNIQUE constraint, so existing databases need a full table rebuild. Legacy
// rows are preserved with lang=”. Nothing references job_urls, so the rebuild
// is safe; foreign keys are disabled around it per the SQLite-recommended
// table-rebuild procedure.
func (s *JobStore) migrateJobURLsLang(ctx context.Context) error {
	has, err := s.hasColumn(ctx, "job_urls", "lang")
	if err != nil {
		return err
	}
	if has {
		return nil
	}

	if _, err := s.db.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return err
	}
	defer func() { _, _ = s.db.ExecContext(ctx, `PRAGMA foreign_keys=ON`) }()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmts := []string{
		`CREATE TABLE job_urls_new (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			job_id TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
			position INTEGER NOT NULL,
			url TEXT NOT NULL,
			lang TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL,
			attempts INTEGER NOT NULL DEFAULT 0,
			last_error TEXT,
			started_at DATETIME,
			finished_at DATETIME,
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL,
			UNIQUE(job_id, position),
			UNIQUE(job_id, url, lang)
		)`,
		`INSERT INTO job_urls_new
			(id, job_id, position, url, lang, status, attempts, last_error, started_at, finished_at, created_at, updated_at)
			SELECT id, job_id, position, url, '', status, attempts, last_error, started_at, finished_at, created_at, updated_at
			FROM job_urls`,
		`DROP TABLE job_urls`,
		`ALTER TABLE job_urls_new RENAME TO job_urls`,
		`CREATE INDEX IF NOT EXISTS idx_job_urls_claim ON job_urls(job_id, status, position)`,
		`CREATE INDEX IF NOT EXISTS idx_job_urls_url_status ON job_urls(url, status)`,
	}
	for _, stmt := range stmts {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// validateSQLIdentifier rejects names containing anything outside letters,
// digits, and underscores so they can be safely embedded in DDL statements
// where parameterized placeholders are not supported.
func validateSQLIdentifier(s string) error {
	if s == "" {
		return fmt.Errorf("SQL identifier must not be empty")
	}
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_') {
			return fmt.Errorf("invalid SQL identifier %q: only letters, digits, and underscores are allowed", s)
		}
	}
	return nil
}

func (s *JobStore) ensureColumn(ctx context.Context, table, column, decl string) error {
	if err := validateSQLIdentifier(table); err != nil {
		return fmt.Errorf("ensureColumn table: %w", err)
	}
	if err := validateSQLIdentifier(column); err != nil {
		return fmt.Errorf("ensureColumn column: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `ALTER TABLE `+table+` ADD COLUMN `+column+` `+decl)
	return err
}

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

func (s *JobStore) CreateStrategyJobsWithSource(ctx context.Context, jobs []StrategyJobCreate) ([]string, string, error) {
	if len(jobs) == 0 {
		return nil, "", nil
	}
	type encodedJob struct {
		queriesJSON string
		configJSON  string
		job         StrategyJobCreate
	}
	encoded := make([]encodedJob, 0, len(jobs))
	for _, job := range jobs {
		queriesJSON, err := json.Marshal(job.Queries)
		if err != nil {
			return nil, "", err
		}
		configJSON, err := json.Marshal(job.Config)
		if err != nil {
			return nil, "", err
		}
		encoded = append(encoded, encodedJob{
			queriesJSON: string(queriesJSON),
			configJSON:  string(configJSON),
			job:         job,
		})
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback()
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE status IN (?, ?)`,
		JobStatusStarting, JobStatusRunning).Scan(&active); err != nil {
		return nil, "", err
	}

	ids := make([]string, 0, len(encoded))
	var startedID string
	var previous time.Time
	for i, job := range encoded {
		now := time.Now().UTC()
		if !previous.IsZero() && !now.After(previous) {
			now = previous.Add(time.Nanosecond)
		}
		previous = now
		id := newJobID(now)
		status := JobStatusPending
		event := "queued"
		message := "job queued by strategy"
		if i == 0 && active == 0 {
			status = JobStatusStarting
			event = "starting"
			message = "strategy job accepted by control UI"
			startedID = id
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO jobs
			(id, queries_json, config_json, status, template_id, strategy_id, strategy_run_id, created_at, updated_at)
			VALUES (?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), ?, ?)`,
			id, job.queriesJSON, job.configJSON, status, job.job.TemplateID, job.job.StrategyID, job.job.StrategyRunID, now, now); err != nil {
			return nil, "", err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO job_events (job_id, event, message, created_at) VALUES (?, ?, ?, ?)`,
			id, event, message, now); err != nil {
			return nil, "", err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO job_execution_stats (job_id, updated_at) VALUES (?, ?)`, id, now); err != nil {
			return nil, "", err
		}
		ids = append(ids, id)
	}
	return ids, startedID, tx.Commit()
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
			AND EXISTS (SELECT 1 FROM job_urls ju WHERE ju.job_id = j.id AND ju.status = ?)
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
type JobProcess struct {
	JobID     string
	PID       int
	PGID      int
	StartedAt time.Time
}

// RecordJobProcess stores (or replaces) the process-group registry row for a
// job after its subprocess starts.
func (s *JobStore) RecordJobProcess(ctx context.Context, jobID string, pid, pgid int) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO job_processes (job_id, pid, pgid, started_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(job_id) DO UPDATE SET pid = excluded.pid, pgid = excluded.pgid, started_at = excluded.started_at`,
		jobID, pid, pgid, time.Now().UTC())
	return err
}

// DeleteJobProcess removes a job's registry row after a clean exit.
func (s *JobStore) DeleteJobProcess(ctx context.Context, jobID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM job_processes WHERE job_id = ?`, jobID)
	return err
}

// ListJobProcesses returns all recorded process groups. After clean exits only
// leaked groups remain.
func (s *JobStore) ListJobProcesses(ctx context.Context) ([]JobProcess, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT job_id, pid, pgid, started_at FROM job_processes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var procs []JobProcess
	for rows.Next() {
		var p JobProcess
		if err := rows.Scan(&p.JobID, &p.PID, &p.PGID, &p.StartedAt); err != nil {
			return nil, err
		}
		procs = append(procs, p)
	}
	return procs, rows.Err()
}

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
		scraped_urls, duplicate_places, scrape_errors, write_errors, retry_events, updated_at
		FROM job_execution_stats WHERE job_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, err
	}
	for execRows.Next() {
		var es JobExecutionStats
		if err := execRows.Scan(&es.JobID, &es.FeedURLsFound, &es.FeedDuplicateURLs,
			&es.CrossJobDuplicateURLs, &es.QueuedURLs, &es.ScrapedURLs, &es.DuplicatePlaces, &es.ScrapeErrors, &es.WriteErrors,
			&es.RetryEvents, &es.UpdatedAt); err != nil {
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

func (s *JobStore) SaveJobTemplateJSON(ctx context.Context, id, name, paramsJSON string) (string, error) {
	now := time.Now().UTC()
	paramsJSON = strings.TrimSpace(paramsJSON)
	if paramsJSON == "" {
		paramsJSON = "{}"
	}
	if !json.Valid([]byte(paramsJSON)) {
		return "", errors.New("template params must be valid JSON")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		sum := sha256.Sum256([]byte(paramsJSON))
		id = fmt.Sprintf("tpl_%x", sum[:12])
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = id
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO job_templates (id, name, params_json, created_at, last_used_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			params_json = excluded.params_json,
			last_used_at = excluded.last_used_at`,
		id, name, paramsJSON, now, now)
	if err != nil {
		return "", err
	}
	return id, nil
}

func (s *JobStore) GetJobTemplate(ctx context.Context, id string) (*JobTemplate, error) {
	var tpl JobTemplate
	err := s.db.QueryRowContext(ctx, `SELECT id, name, params_json, created_at, last_used_at
		FROM job_templates WHERE id = ?`, id).Scan(&tpl.ID, &tpl.Name, &tpl.ParamsJSON, &tpl.CreatedAt, &tpl.LastUsedAt)
	if err != nil {
		return nil, err
	}
	return &tpl, nil
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

func (s *JobStore) DeleteJobTemplate(ctx context.Context, id string) error {
	var refs int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM strategy_templates WHERE template_id = ?`, id).Scan(&refs); err != nil {
		return err
	}
	if refs > 0 {
		return fmt.Errorf("%w: used by %d strategy entries", ErrTemplateReferenced, refs)
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM job_templates WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// TemplateDeleteResult reports the outcome of deleting one template in a batch.
// Status is one of "deleted", "skipped_referenced", or "not_found".
type TemplateDeleteResult struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// BatchDeleteJobTemplates deletes each template by reusing DeleteJobTemplate,
// which refuses templates still referenced by a strategy. Referenced templates
// are reported as "skipped_referenced", unknown ids as "not_found"; the rest as
// "deleted".
func (s *JobStore) BatchDeleteJobTemplates(ctx context.Context, ids []string) ([]TemplateDeleteResult, error) {
	results := make([]TemplateDeleteResult, 0, len(ids))
	for _, id := range ids {
		status := "deleted"
		switch err := s.DeleteJobTemplate(ctx, id); {
		case err == nil:
		case errors.Is(err, sql.ErrNoRows):
			status = "not_found"
		case errors.Is(err, ErrTemplateReferenced):
			status = "skipped_referenced"
		default:
			return nil, err
		}
		results = append(results, TemplateDeleteResult{ID: id, Status: status})
	}
	return results, nil
}

func (s *JobStore) SaveStrategy(ctx context.Context, id, name, notes string, templateIDs []string) (string, error) {
	now := time.Now().UTC()
	id = strings.TrimSpace(id)
	if id == "" {
		id = newStrategyID(now)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("strategy name is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO strategies (id, name, notes, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET name = excluded.name, notes = excluded.notes, updated_at = excluded.updated_at`,
		id, name, strings.TrimSpace(notes), now, now); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM strategy_templates WHERE strategy_id = ?`, id); err != nil {
		return "", err
	}
	seen := make(map[string]struct{}, len(templateIDs))
	position := 0
	for _, templateID := range templateIDs {
		templateID = strings.TrimSpace(templateID)
		if templateID == "" {
			continue
		}
		if _, ok := seen[templateID]; ok {
			continue
		}
		seen[templateID] = struct{}{}
		if _, err := tx.ExecContext(ctx, `INSERT INTO strategy_templates (strategy_id, template_id, position, created_at)
			VALUES (?, ?, ?, ?)`, id, templateID, position, now); err != nil {
			return "", err
		}
		position++
	}
	return id, tx.Commit()
}

func (s *JobStore) GetStrategy(ctx context.Context, id string) (*Strategy, error) {
	var strategy Strategy
	err := s.db.QueryRowContext(ctx, `SELECT id, name, notes, created_at, updated_at, last_used_at
		FROM strategies WHERE id = ?`, id).Scan(&strategy.ID, &strategy.Name, &strategy.Notes, &strategy.CreatedAt, &strategy.UpdatedAt, &strategy.LastUsedAt)
	if err != nil {
		return nil, err
	}
	templates, err := s.listStrategyTemplates(ctx, id)
	if err != nil {
		return nil, err
	}
	strategy.Templates = templates
	return &strategy, nil
}

func (s *JobStore) ListStrategies(ctx context.Context) ([]Strategy, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, notes, created_at, updated_at, last_used_at
		FROM strategies ORDER BY updated_at DESC, created_at DESC`)
	if err != nil {
		return nil, err
	}
	var strategies []Strategy
	for rows.Next() {
		var strategy Strategy
		if err := rows.Scan(&strategy.ID, &strategy.Name, &strategy.Notes, &strategy.CreatedAt, &strategy.UpdatedAt, &strategy.LastUsedAt); err != nil {
			rows.Close()
			return nil, err
		}
		strategies = append(strategies, strategy)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range strategies {
		templates, err := s.listStrategyTemplates(ctx, strategies[i].ID)
		if err != nil {
			return nil, err
		}
		strategies[i].Templates = templates
	}
	return strategies, nil
}

func (s *JobStore) listStrategyTemplates(ctx context.Context, strategyID string) ([]JobTemplate, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT jt.id, jt.name, jt.params_json, jt.created_at, jt.last_used_at
		FROM strategy_templates st
		JOIN job_templates jt ON jt.id = st.template_id
		WHERE st.strategy_id = ?
		ORDER BY st.position ASC`, strategyID)
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

func (s *JobStore) ExportReusableConfig(ctx context.Context) (ReusableConfigExport, error) {
	return s.ExportReusableConfigSelection(ctx, nil, nil)
}

func (s *JobStore) ExportReusableConfigSelection(ctx context.Context, templateIDs, strategyIDs []string) (ReusableConfigExport, error) {
	templates, err := s.ListJobTemplates(ctx)
	if err != nil {
		return ReusableConfigExport{}, err
	}
	strategies, err := s.ListStrategies(ctx)
	if err != nil {
		return ReusableConfigExport{}, err
	}
	selectedTemplates := normalizedIDSet(templateIDs)
	selectedStrategies := normalizedIDSet(strategyIDs)
	filtered := len(selectedTemplates) > 0 || len(selectedStrategies) > 0
	if filtered {
		knownTemplates := make(map[string]struct{}, len(templates))
		for _, tpl := range templates {
			knownTemplates[tpl.ID] = struct{}{}
		}
		for id := range selectedTemplates {
			if _, ok := knownTemplates[id]; !ok {
				return ReusableConfigExport{}, fmt.Errorf("%w: template %q was not found", ErrConfigExportInvalid, id)
			}
		}
		knownStrategies := make(map[string]struct{}, len(strategies))
		for _, strategy := range strategies {
			knownStrategies[strategy.ID] = struct{}{}
		}
		for id := range selectedStrategies {
			if _, ok := knownStrategies[id]; !ok {
				return ReusableConfigExport{}, fmt.Errorf("%w: strategy %q was not found", ErrConfigExportInvalid, id)
			}
		}
		for _, strategy := range strategies {
			if _, ok := selectedStrategies[strategy.ID]; !ok {
				continue
			}
			for _, tpl := range strategy.Templates {
				selectedTemplates[tpl.ID] = struct{}{}
			}
		}
	}
	out := ReusableConfigExport{
		Version:    ConfigExportVersion,
		ExportedAt: time.Now().UTC(),
		Source:     "google-maps-scraper-lite",
		Templates:  make([]ReusableConfigTemplate, 0, len(templates)),
		Strategies: make([]ReusableConfigStrategy, 0, len(strategies)),
	}
	for _, tpl := range templates {
		if filtered {
			if _, ok := selectedTemplates[tpl.ID]; !ok {
				continue
			}
		}
		out.Templates = append(out.Templates, ReusableConfigTemplate{
			ID:         tpl.ID,
			Name:       tpl.Name,
			ParamsJSON: tpl.ParamsJSON,
			CreatedAt:  tpl.CreatedAt,
			LastUsedAt: tpl.LastUsedAt,
		})
	}
	for _, strategy := range strategies {
		if filtered {
			if _, ok := selectedStrategies[strategy.ID]; !ok {
				continue
			}
		}
		templateIDs := make([]string, 0, len(strategy.Templates))
		for _, tpl := range strategy.Templates {
			templateIDs = append(templateIDs, tpl.ID)
		}
		exported := ReusableConfigStrategy{
			ID:          strategy.ID,
			Name:        strategy.Name,
			Notes:       strategy.Notes,
			TemplateIDs: templateIDs,
			CreatedAt:   strategy.CreatedAt,
			UpdatedAt:   strategy.UpdatedAt,
		}
		if strategy.LastUsedAt.Valid {
			exported.LastUsedAt = strategy.LastUsedAt.Time
		}
		out.Strategies = append(out.Strategies, exported)
	}
	return out, nil
}

func normalizedIDSet(ids []string) map[string]struct{} {
	out := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id != "" {
			out[id] = struct{}{}
		}
	}
	return out
}

func (s *JobStore) ImportReusableConfig(ctx context.Context, cfg ReusableConfigExport, mode ConfigImportMode) (ConfigImportSummary, error) {
	if mode == "" {
		mode = ConfigImportRename
	}
	if !validConfigImportMode(mode) {
		return ConfigImportSummary{}, fmt.Errorf("%w: unsupported collision mode %q", ErrConfigImportInvalid, mode)
	}
	if err := validateReusableConfig(cfg); err != nil {
		return ConfigImportSummary{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ConfigImportSummary{}, err
	}
	defer tx.Rollback()

	summary := ConfigImportSummary{Mode: mode}
	now := time.Now().UTC()
	existingTemplates, err := txStringSet(ctx, tx, `SELECT id FROM job_templates`)
	if err != nil {
		return ConfigImportSummary{}, err
	}
	existingStrategies, err := txStringSet(ctx, tx, `SELECT id FROM strategies`)
	if err != nil {
		return ConfigImportSummary{}, err
	}

	templatePayload := make(map[string]ReusableConfigTemplate, len(cfg.Templates))
	templateMap := make(map[string]string, len(cfg.Templates))
	usedTemplateIDs := copyStringSet(existingTemplates)
	for _, tpl := range cfg.Templates {
		templatePayload[tpl.ID] = tpl
		targetID := tpl.ID
		_, exists := existingTemplates[tpl.ID]
		switch mode {
		case ConfigImportSkip:
			if exists {
				summary.Templates.Skipped++
				templateMap[tpl.ID] = tpl.ID
				continue
			}
			summary.Templates.Created++
		case ConfigImportOverwrite:
			if exists {
				summary.Templates.Updated++
			} else {
				summary.Templates.Created++
			}
		case ConfigImportRename:
			if exists {
				targetID = uniqueImportID("tpl", tpl.ID, usedTemplateIDs)
				summary.Templates.Renamed++
				summary.Templates.Created++
			} else {
				summary.Templates.Created++
			}
		case ConfigImportDuplicate:
			targetID = uniqueImportID("tpl", tpl.ID, usedTemplateIDs)
			summary.Templates.Duplicated++
			summary.Templates.Created++
		}
		usedTemplateIDs[targetID] = struct{}{}
		templateMap[tpl.ID] = targetID
		name := importName(tpl.Name, targetID, targetID != tpl.ID, mode)
		if err := upsertTemplateTx(ctx, tx, targetID, name, tpl.ParamsJSON, importTime(tpl.CreatedAt, now), importTime(tpl.LastUsedAt, now)); err != nil {
			return ConfigImportSummary{}, err
		}
	}

	for _, strategy := range cfg.Strategies {
		for _, originalTemplateID := range strategy.TemplateIDs {
			if _, ok := templateMap[originalTemplateID]; ok {
				continue
			}
			if _, ok := templatePayload[originalTemplateID]; ok {
				return ConfigImportSummary{}, fmt.Errorf("%w: strategy %q references skipped template %q", ErrConfigImportConflict, strategy.ID, originalTemplateID)
			}
			if _, ok := existingTemplates[originalTemplateID]; ok {
				templateMap[originalTemplateID] = originalTemplateID
				continue
			}
			return ConfigImportSummary{}, fmt.Errorf("%w: strategy %q references missing template %q", ErrConfigImportInvalid, strategy.ID, originalTemplateID)
		}
	}

	usedStrategyIDs := copyStringSet(existingStrategies)
	for _, strategy := range cfg.Strategies {
		targetID := strategy.ID
		_, exists := existingStrategies[strategy.ID]
		switch mode {
		case ConfigImportSkip:
			if exists {
				summary.Strategies.Skipped++
				continue
			}
			summary.Strategies.Created++
		case ConfigImportOverwrite:
			if exists {
				summary.Strategies.Updated++
			} else {
				summary.Strategies.Created++
			}
		case ConfigImportRename:
			if exists {
				targetID = uniqueImportID("str", strategy.ID, usedStrategyIDs)
				summary.Strategies.Renamed++
				summary.Strategies.Created++
			} else {
				summary.Strategies.Created++
			}
		case ConfigImportDuplicate:
			targetID = uniqueImportID("str", strategy.ID, usedStrategyIDs)
			summary.Strategies.Duplicated++
			summary.Strategies.Created++
		}
		usedStrategyIDs[targetID] = struct{}{}
		templateIDs := make([]string, 0, len(strategy.TemplateIDs))
		for _, originalTemplateID := range strategy.TemplateIDs {
			templateIDs = append(templateIDs, templateMap[originalTemplateID])
		}
		name := importName(strategy.Name, targetID, targetID != strategy.ID, mode)
		if err := upsertStrategyTx(ctx, tx, targetID, name, strategy.Notes, templateIDs, importTime(strategy.CreatedAt, now), importTime(strategy.UpdatedAt, now), strategy.LastUsedAt); err != nil {
			return ConfigImportSummary{}, err
		}
	}

	if err := tx.Commit(); err != nil {
		return ConfigImportSummary{}, err
	}
	return summary, nil
}

func validConfigImportMode(mode ConfigImportMode) bool {
	switch mode {
	case ConfigImportRename, ConfigImportSkip, ConfigImportOverwrite, ConfigImportDuplicate:
		return true
	default:
		return false
	}
}

func validateReusableConfig(cfg ReusableConfigExport) error {
	if cfg.Version != ConfigExportVersion {
		return fmt.Errorf("%w: unsupported version %d", ErrConfigImportInvalid, cfg.Version)
	}
	templateIDs := make(map[string]struct{}, len(cfg.Templates))
	for _, tpl := range cfg.Templates {
		id := strings.TrimSpace(tpl.ID)
		if id == "" {
			return fmt.Errorf("%w: template id is required", ErrConfigImportInvalid)
		}
		if _, ok := templateIDs[id]; ok {
			return fmt.Errorf("%w: duplicate template id %q", ErrConfigImportInvalid, id)
		}
		templateIDs[id] = struct{}{}
		if strings.TrimSpace(tpl.Name) == "" {
			return fmt.Errorf("%w: template %q name is required", ErrConfigImportInvalid, id)
		}
		if !json.Valid([]byte(strings.TrimSpace(tpl.ParamsJSON))) {
			return fmt.Errorf("%w: template %q params_json must be valid JSON", ErrConfigImportInvalid, id)
		}
	}
	strategyIDs := make(map[string]struct{}, len(cfg.Strategies))
	for _, strategy := range cfg.Strategies {
		id := strings.TrimSpace(strategy.ID)
		if id == "" {
			return fmt.Errorf("%w: strategy id is required", ErrConfigImportInvalid)
		}
		if _, ok := strategyIDs[id]; ok {
			return fmt.Errorf("%w: duplicate strategy id %q", ErrConfigImportInvalid, id)
		}
		strategyIDs[id] = struct{}{}
		if strings.TrimSpace(strategy.Name) == "" {
			return fmt.Errorf("%w: strategy %q name is required", ErrConfigImportInvalid, id)
		}
	}
	return nil
}

func txStringSet(ctx context.Context, tx *sql.Tx, query string) (map[string]struct{}, error) {
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]struct{})
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = struct{}{}
	}
	return out, rows.Err()
}

func copyStringSet(in map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(in))
	for k := range in {
		out[k] = struct{}{}
	}
	return out
}

func uniqueImportID(prefix, original string, used map[string]struct{}) string {
	base := strings.TrimSpace(original)
	for i := 1; ; i++ {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%d", prefix, base, i)))
		id := fmt.Sprintf("%s_%x", prefix, sum[:8])
		if _, ok := used[id]; !ok {
			return id
		}
	}
}

func importName(name, targetID string, changed bool, mode ConfigImportMode) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = targetID
	}
	if !changed {
		return name
	}
	suffix := " imported"
	if mode == ConfigImportDuplicate {
		suffix = " copy"
	}
	if strings.HasSuffix(name, suffix) {
		return name
	}
	return strings.TrimSpace(name + suffix)
}

func importTime(value, fallback time.Time) time.Time {
	if value.IsZero() {
		return fallback
	}
	return value.UTC()
}

func upsertTemplateTx(ctx context.Context, tx *sql.Tx, id, name, paramsJSON string, createdAt, lastUsedAt time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO job_templates (id, name, params_json, created_at, last_used_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			params_json = excluded.params_json,
			last_used_at = excluded.last_used_at`,
		id, name, paramsJSON, createdAt, lastUsedAt)
	return err
}

func upsertStrategyTx(ctx context.Context, tx *sql.Tx, id, name, notes string, templateIDs []string, createdAt, updatedAt, lastUsedAt time.Time) error {
	var lastUsed any
	if !lastUsedAt.IsZero() {
		lastUsed = lastUsedAt.UTC()
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO strategies (id, name, notes, created_at, updated_at, last_used_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			notes = excluded.notes,
			updated_at = excluded.updated_at,
			last_used_at = excluded.last_used_at`,
		id, name, strings.TrimSpace(notes), createdAt, updatedAt, lastUsed); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM strategy_templates WHERE strategy_id = ?`, id); err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(templateIDs))
	for position, templateID := range templateIDs {
		templateID = strings.TrimSpace(templateID)
		if templateID == "" {
			continue
		}
		if _, ok := seen[templateID]; ok {
			continue
		}
		seen[templateID] = struct{}{}
		if _, err := tx.ExecContext(ctx, `INSERT INTO strategy_templates (strategy_id, template_id, position, created_at)
			VALUES (?, ?, ?, ?)`, id, templateID, position, updatedAt); err != nil {
			return err
		}
	}
	return nil
}

// DeleteStrategy removes a strategy. When deleteTemplates is true it also
// deletes the strategy's job templates that become unreferenced once this
// strategy's join rows are gone; templates still shared with another strategy
// are kept (only their join row to this strategy is removed via the
// strategy_templates ON DELETE CASCADE foreign key).
func (s *JobStore) DeleteStrategy(ctx context.Context, id string, deleteTemplates bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var templateIDs []string
	if deleteTemplates {
		rows, err := tx.QueryContext(ctx, `SELECT template_id FROM strategy_templates WHERE strategy_id = ?`, id)
		if err != nil {
			return err
		}
		for rows.Next() {
			var tplID string
			if err := rows.Scan(&tplID); err != nil {
				rows.Close()
				return err
			}
			templateIDs = append(templateIDs, tplID)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}

	res, err := tx.ExecContext(ctx, `DELETE FROM strategies WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}

	// Strategy is gone (its join rows cascaded). Delete only templates that are
	// now unreferenced; shared templates still have a row for another strategy.
	for _, tplID := range templateIDs {
		var refs int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM strategy_templates WHERE template_id = ?`, tplID).Scan(&refs); err != nil {
			return err
		}
		if refs > 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM job_templates WHERE id = ?`, tplID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *JobStore) MarkStrategyUsed(ctx context.Context, id string) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `UPDATE strategies SET last_used_at = ?, updated_at = ? WHERE id = ?`, now, now, id)
	return err
}

// BulkUpdateStrategyLang updates the Lang field in-place for every template
// belonging to the given strategy. Because templates may be shared across
// strategies, this affects all strategies that reference the same templates.
func (s *JobStore) BulkUpdateStrategyLang(ctx context.Context, strategyID, lang string) (int, error) {
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM strategies WHERE id = ?`, strategyID).Scan(&exists); err != nil {
		return 0, err
	}
	if exists == 0 {
		return 0, sql.ErrNoRows
	}

	rows, err := s.db.QueryContext(ctx, `SELECT template_id FROM strategy_templates WHERE strategy_id = ?`, strategyID)
	if err != nil {
		return 0, err
	}
	var templateIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		templateIDs = append(templateIDs, id)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	now := time.Now().UTC()
	for _, tplID := range templateIDs {
		var paramsJSON string
		if err := tx.QueryRowContext(ctx, `SELECT params_json FROM job_templates WHERE id = ?`, tplID).Scan(&paramsJSON); err != nil {
			return 0, err
		}
		var params map[string]any
		if err := json.Unmarshal([]byte(paramsJSON), &params); err != nil {
			return 0, err
		}
		if lang == "" {
			delete(params, "Lang")
		} else {
			params["Lang"] = lang
		}
		updated, err := json.Marshal(params)
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE job_templates SET params_json = ?, last_used_at = ? WHERE id = ?`,
			string(updated), now, tplID); err != nil {
			return 0, err
		}
	}
	return len(templateIDs), tx.Commit()
}

// BulkUpdateStrategyDedupScope updates the DedupScope field in-place for every
// template belonging to the given strategy. Empty scope disables the option.
func (s *JobStore) BulkUpdateStrategyDedupScope(ctx context.Context, strategyID, scope string) (int, error) {
	if scope != "" && scope != "run" && scope != "all" {
		return 0, fmt.Errorf("invalid dedup scope %q", scope)
	}
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM strategies WHERE id = ?`, strategyID).Scan(&exists); err != nil {
		return 0, err
	}
	if exists == 0 {
		return 0, sql.ErrNoRows
	}

	rows, err := s.db.QueryContext(ctx, `SELECT template_id FROM strategy_templates WHERE strategy_id = ?`, strategyID)
	if err != nil {
		return 0, err
	}
	var templateIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		templateIDs = append(templateIDs, id)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	now := time.Now().UTC()
	for _, tplID := range templateIDs {
		var paramsJSON string
		if err := tx.QueryRowContext(ctx, `SELECT params_json FROM job_templates WHERE id = ?`, tplID).Scan(&paramsJSON); err != nil {
			return 0, err
		}
		var params map[string]any
		if err := json.Unmarshal([]byte(paramsJSON), &params); err != nil {
			return 0, err
		}
		if scope == "" {
			delete(params, "DedupScope")
		} else {
			params["DedupScope"] = scope
		}
		updated, err := json.Marshal(params)
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE job_templates SET params_json = ?, last_used_at = ? WHERE id = ?`,
			string(updated), now, tplID); err != nil {
			return 0, err
		}
	}
	return len(templateIDs), tx.Commit()
}

// BulkDuplicateStrategyTemplatesWithLang clones every template in the strategy
// with the new lang and nameSuffix appended to each template name, then
// re-links the strategy to the new copies. Original templates are untouched.
func (s *JobStore) BulkDuplicateStrategyTemplatesWithLang(ctx context.Context, strategyID, lang, nameSuffix string) (int, error) {
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM strategies WHERE id = ?`, strategyID).Scan(&exists); err != nil {
		return 0, err
	}
	if exists == 0 {
		return 0, sql.ErrNoRows
	}

	type entry struct {
		templateID string
		position   int
		name       string
		paramsJSON string
	}
	rows, err := s.db.QueryContext(ctx, `SELECT st.template_id, st.position, jt.name, jt.params_json
		FROM strategy_templates st
		JOIN job_templates jt ON jt.id = st.template_id
		WHERE st.strategy_id = ?
		ORDER BY st.position ASC`, strategyID)
	if err != nil {
		return 0, err
	}
	var entries []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.templateID, &e.position, &e.name, &e.paramsJSON); err != nil {
			rows.Close()
			return 0, err
		}
		entries = append(entries, e)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	now := time.Now().UTC()
	for _, e := range entries {
		var params map[string]any
		if err := json.Unmarshal([]byte(e.paramsJSON), &params); err != nil {
			return 0, err
		}
		if lang == "" {
			delete(params, "Lang")
		} else {
			params["Lang"] = lang
		}
		newParamsJSON, err := json.Marshal(params)
		if err != nil {
			return 0, err
		}
		sum := sha256.Sum256(newParamsJSON)
		newID := fmt.Sprintf("tpl_%x", sum[:12])
		newName := strings.TrimSpace(e.name) + " " + strings.TrimSpace(nameSuffix)

		if _, err := tx.ExecContext(ctx, `INSERT INTO job_templates (id, name, params_json, created_at, last_used_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET name = excluded.name, last_used_at = excluded.last_used_at`,
			newID, newName, string(newParamsJSON), now, now); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM strategy_templates WHERE strategy_id = ? AND template_id = ?`,
			strategyID, e.templateID); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO strategy_templates (strategy_id, template_id, position, created_at)
			VALUES (?, ?, ?, ?)`, strategyID, newID, e.position, now); err != nil {
			return 0, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE strategies SET updated_at = ? WHERE id = ?`, now, strategyID); err != nil {
		return 0, err
	}
	return len(entries), tx.Commit()
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
			template_id, strategy_id, strategy_run_id,
			created_at, started_at, updated_at, finished_at, last_error`,
		JobStatusRunning, now, now, jobID, JobStatusStarting, JobStatusRunning, JobStatusDone)
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
		} else if err := s.SetJobStatus(ctx, jobID, JobStatusFailed, staleErr); err != nil {
			return err
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
		RETURNING id, position, url, lang`, URLStatusInProgress, now, now, jobID, URLStatusPending)
	var claimed ClaimedURL
	claimed.JobID = jobID
	if err := row.Scan(&claimed.ID, &claimed.Position, &claimed.URL, &claimed.Lang); err != nil {
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

// FilterAlreadyScrapedURLs removes (url, lang) pairs already marked done in
// prior job rows. Matching is per-(url, lang): the same place URL in a not-yet
// scraped language is kept. Legacy done rows predate the lang column and carry
// lang=”; they are treated as firstLang so pre-existing single-language data is
// still recognized. Run-scoped filtering needs a strategy run id; without one it
// is a no-op so ad-hoc CLI jobs keep their historical behavior unless all-time
// mode is used.
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

func newJobID(now time.Time) string {
	return fmt.Sprintf("job_%s_%09d", now.Format("20060102_150405"), now.Nanosecond())
}

func newStrategyID(now time.Time) string {
	return fmt.Sprintf("str_%s_%09d", now.Format("20060102_150405"), now.Nanosecond())
}

func NewStrategyRunID(now time.Time) string {
	return fmt.Sprintf("run_%s_%09d", now.Format("20060102_150405"), now.Nanosecond())
}
