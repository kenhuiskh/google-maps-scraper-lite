package gmaps

import (
	"database/sql"
	"errors"
	"time"
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
	Attempts int
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
