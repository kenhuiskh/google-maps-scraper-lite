package main

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/gosom/google-maps-scraper-lite/gmaps"
)

type controlPageData struct {
	Summary       controlSummaryView
	Jobs          []jobView
	Pagination    jobsPaginationView
	Templates     []gmaps.JobTemplate
	LastRefreshed string
}

type jobsPaginationView struct {
	Page            int
	PageSize        int
	Filter          string
	FilterLabel     string
	TotalJobs       int
	TotalPages      int
	Offset          int
	StartItem       int
	EndItem         int
	HasPrevious     bool
	HasNext         bool
	PreviousPage    int
	NextPage        int
	PageSizeOptions []pageSizeOption
	FilterOptions   []jobsFilterOption
}

type pageSizeOption struct {
	Value    int
	Selected bool
}

type jobsFilterOption struct {
	Value    string
	Label    string
	Selected bool
}

type jobView struct {
	ID              string
	QueriesPreview  string
	CreatedAt       string
	UpdatedAt       string
	StatusLabel     string
	StatusClass     string
	StatusHelp      string
	Progress        string
	ProgressPercent int
	Total           int
	Pending         int
	InProgress      int
	Done            int
	Failed          int
	LastError       string
	ActionLabel     string
	ActionPath      string
	ActionDisabled  bool
	ActionClass     string
	ActionHelp      string
	RawStatus       string
	PauseRequested  bool
	OutputMode      string
	Lang            string
	Active          bool
	Blocked         bool
}

type controlSummaryView struct {
	ActiveJobID       string
	ActiveJobTitle    string
	ActiveStatus      string
	ActiveStatusClass string
	ActiveProgress    string
	HasActiveJob      bool
	PendingJobs       int
	RunningJobs       int
	StartingJobs      int
	CompletedJobs     int
	FailedJobs        int
	BlockedJobs       int
	NeedsAttention    int
	TotalJobs         int
	PendingURLs       int
	ActiveURLs        int
}

func newControlPageData(jobs []gmaps.Job, templates []gmaps.JobTemplate) controlPageData {
	return newControlPageDataWithPagination(jobs, jobs, templates, newJobsPagination(1, defaultJobsPageSize, len(jobs), defaultJobsFilter))
}

func newControlPageDataWithPagination(summaryJobs, pageJobs []gmaps.Job, templates []gmaps.JobTemplate, pagination jobsPaginationView) controlPageData {
	views := make([]jobView, 0, len(pageJobs))
	for _, job := range pageJobs {
		views = append(views, newJobView(job))
	}
	return controlPageData{
		Summary:       newControlSummaryView(summaryJobs),
		Jobs:          views,
		Pagination:    pagination,
		Templates:     templates,
		LastRefreshed: time.Now().Format("15:04:05"),
	}
}

const defaultJobsPageSize = 10

var allowedJobsPageSizes = []int{10, 25, 50}

func newJobsPagination(page, pageSize, totalJobs int, filter string) jobsPaginationView {
	pageSize = normalizeJobsPageSize(pageSize)
	filter = normalizeJobsFilter(filter)
	totalPages := 1
	if totalJobs > 0 {
		totalPages = (totalJobs + pageSize - 1) / pageSize
	}
	if page < 1 {
		page = 1
	}
	if page > totalPages {
		page = totalPages
	}
	offset := (page - 1) * pageSize
	startItem := 0
	endItem := 0
	if totalJobs > 0 {
		startItem = offset + 1
		endItem = offset + pageSize
		if endItem > totalJobs {
			endItem = totalJobs
		}
	}
	options := make([]pageSizeOption, 0, len(allowedJobsPageSizes))
	for _, size := range allowedJobsPageSizes {
		options = append(options, pageSizeOption{Value: size, Selected: size == pageSize})
	}
	filterOptions := make([]jobsFilterOption, 0, len(allowedJobsFilters))
	for _, opt := range allowedJobsFilters {
		filterOptions = append(filterOptions, jobsFilterOption{
			Value:    opt.Value,
			Label:    opt.Label,
			Selected: opt.Value == filter,
		})
	}
	return jobsPaginationView{
		Page:            page,
		PageSize:        pageSize,
		Filter:          filter,
		FilterLabel:     jobsFilterLabel(filter),
		TotalJobs:       totalJobs,
		TotalPages:      totalPages,
		Offset:          offset,
		StartItem:       startItem,
		EndItem:         endItem,
		HasPrevious:     page > 1,
		HasNext:         page < totalPages,
		PreviousPage:    page - 1,
		NextPage:        page + 1,
		PageSizeOptions: options,
		FilterOptions:   filterOptions,
	}
}

func normalizeJobsPageSize(pageSize int) int {
	for _, allowed := range allowedJobsPageSizes {
		if pageSize == allowed {
			return pageSize
		}
	}
	return defaultJobsPageSize
}

const defaultJobsFilter = "all"

var allowedJobsFilters = []jobsFilterOption{
	{Value: "all", Label: "All"},
	{Value: "pending", Label: "Pending"},
	{Value: "active", Label: "Active"},
	{Value: "done", Label: "Done"},
}

func normalizeJobsFilter(filter string) string {
	for _, allowed := range allowedJobsFilters {
		if filter == allowed.Value {
			return filter
		}
	}
	return defaultJobsFilter
}

func jobsFilterLabel(filter string) string {
	filter = normalizeJobsFilter(filter)
	for _, allowed := range allowedJobsFilters {
		if filter == allowed.Value {
			return allowed.Label
		}
	}
	return "All"
}

func newJobView(job gmaps.Job) jobView {
	label, class, help := jobStatusDisplay(job)
	cfg := jobConfigView(job.ConfigJSON)
	percent := 0
	if job.Stats.Total > 0 {
		percent = int(float64(job.Stats.Done) / float64(job.Stats.Total) * 100)
	}
	view := jobView{
		ID:              job.ID,
		QueriesPreview:  formatQueriesPreview(job.Queries),
		CreatedAt:       formatControlTime(job.CreatedAt),
		UpdatedAt:       formatControlTime(job.UpdatedAt),
		StatusLabel:     label,
		StatusClass:     class,
		StatusHelp:      help,
		Progress:        formatJobProgress(job.Stats),
		ProgressPercent: percent,
		Total:           job.Stats.Total,
		Pending:         job.Stats.Pending,
		InProgress:      job.Stats.InProgress,
		Done:            job.Stats.Done,
		Failed:          job.Stats.Failed,
		RawStatus:       job.Status,
		PauseRequested:  job.PauseRequested,
		OutputMode:      cfg.OutputMode,
		Lang:            cfg.Lang,
		Active:          job.Status == gmaps.JobStatusStarting || job.Status == gmaps.JobStatusRunning,
		Blocked:         job.Status == gmaps.JobStatusBlocked || job.Status == gmaps.JobStatusFailed,
	}
	view.ActionLabel, view.ActionPath, view.ActionDisabled, view.ActionClass, view.ActionHelp = jobLifecycleAction(job)
	if job.LastError.Valid {
		view.LastError = job.LastError.String
	}
	return view
}

func jobLifecycleAction(job gmaps.Job) (label, path string, disabled bool, class, help string) {
	switch {
	case job.Status == gmaps.JobStatusRunning && job.PauseRequested:
		return "Pausing", "", true, "action-muted", "Pause has already been requested; active scrapes are finishing."
	case job.Status == gmaps.JobStatusRunning:
		return "Pause", "/api/jobs/" + job.ID + "/pause", false, "action-warning", "Request a graceful pause. Active scrapes finish before the job becomes paused."
	case job.Status == gmaps.JobStatusPaused || job.Status == gmaps.JobStatusBlocked || job.Status == gmaps.JobStatusFailed:
		return "Resume", "/api/jobs/" + job.ID + "/resume", false, "action-primary", "Start a new scraper process and continue from saved pending URLs."
	case job.Status == gmaps.JobStatusStarting:
		return "Starting", "", true, "action-muted", "The job is collecting Google Maps result URLs."
	case job.Status == gmaps.JobStatusPending:
		return "Queued", "", true, "action-muted", "This job is waiting behind the active job."
	case job.Status == gmaps.JobStatusDone:
		return "Done", "", true, "action-muted", "This job has finished."
	default:
		return "Unavailable", "", true, "action-muted", "No lifecycle action is available for this job state."
	}
}

func newControlSummaryView(jobs []gmaps.Job) controlSummaryView {
	var summary controlSummaryView
	summary.TotalJobs = len(jobs)
	for _, job := range jobs {
		switch job.Status {
		case gmaps.JobStatusStarting:
			summary.StartingJobs++
		case gmaps.JobStatusRunning:
			summary.RunningJobs++
		case gmaps.JobStatusPending:
			summary.PendingJobs++
		case gmaps.JobStatusDone:
			summary.CompletedJobs++
		case gmaps.JobStatusFailed:
			summary.FailedJobs++
		case gmaps.JobStatusBlocked:
			summary.BlockedJobs++
		}
		summary.NeedsAttention = summary.BlockedJobs + summary.FailedJobs
		summary.PendingURLs += job.Stats.Pending
		summary.ActiveURLs += job.Stats.InProgress
		if !summary.HasActiveJob && (job.Status == gmaps.JobStatusStarting || job.Status == gmaps.JobStatusRunning) {
			view := newJobView(job)
			summary.HasActiveJob = true
			summary.ActiveJobID = job.ID
			summary.ActiveJobTitle = view.QueriesPreview
			summary.ActiveStatus = view.StatusLabel
			summary.ActiveStatusClass = view.StatusClass
			summary.ActiveProgress = view.Progress
		}
	}
	return summary
}

func formatQueriesPreview(queries []string) string {
	if len(queries) == 0 {
		return "No query saved"
	}
	first := strings.TrimSpace(queries[0])
	if first == "" {
		first = "Untitled query"
	}
	if len(first) > 72 {
		first = first[:69] + "..."
	}
	if len(queries) > 1 {
		return first + " +" + strconv.Itoa(len(queries)-1)
	}
	return first
}

func formatControlTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("Jan 02 15:04")
}

func jobConfigView(configJSON string) gmaps.Config {
	var cfg gmaps.Config
	_ = json.Unmarshal([]byte(configJSON), &cfg)
	if cfg.OutputMode == "" {
		cfg.OutputMode = "file"
	}
	if cfg.Lang == "" {
		cfg.Lang = "en"
	}
	return cfg
}

func jobStatusDisplay(job gmaps.Job) (label, class, help string) {
	if job.Status == gmaps.JobStatusRunning && job.PauseRequested {
		return "Pausing", "status-pausing", "Pause requested; active scrapes are finishing before the process exits."
	}
	switch job.Status {
	case gmaps.JobStatusStarting:
		return "Starting", "status-starting", "Collecting Google Maps result URLs before place scraping begins."
	case gmaps.JobStatusRunning:
		return "Running", "status-running", "Scraping is active."
	case gmaps.JobStatusPaused:
		return "Paused", "status-paused", "Stopped safely; resume continues from saved pending URLs."
	case gmaps.JobStatusBlocked:
		return "Blocked", "status-blocked", "Google likely rate-limited or blocked the browser session; resume later."
	case gmaps.JobStatusDone:
		return "Done", "status-done", "All queued URLs were processed."
	case gmaps.JobStatusFailed:
		return "Failed", "status-failed", "The job stopped with an error."
	case gmaps.JobStatusPending:
		return "Pending", "status-pending", "Job is created but has not started yet."
	default:
		if job.Status == "" {
			return "Unknown", "status-unknown", "Job status is missing."
		}
		return job.Status, "status-unknown", "Unrecognized job status."
	}
}

func formatJobProgress(stats gmaps.JobStats) string {
	if stats.Total == 0 {
		return "No URLs queued yet"
	}
	parts := []string{strconv.Itoa(stats.Done) + " / " + strconv.Itoa(stats.Total) + " done"}
	if stats.Pending > 0 {
		parts = append(parts, strconv.Itoa(stats.Pending)+" pending")
	}
	if stats.InProgress > 0 {
		parts = append(parts, strconv.Itoa(stats.InProgress)+" active")
	}
	if stats.Failed > 0 {
		parts = append(parts, strconv.Itoa(stats.Failed)+" failed")
	}
	return strings.Join(parts, ", ")
}
