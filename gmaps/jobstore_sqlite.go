package gmaps

import (
	"context"
	"database/sql"
	"fmt"
	_ "github.com/mattn/go-sqlite3"
	"os"
	"path/filepath"
)

type JobStore struct {
	db *sql.DB
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
