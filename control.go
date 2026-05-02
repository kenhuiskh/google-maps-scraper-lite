package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gosom/google-maps-scraper-lite/gmaps"
)

type resumeLauncher func(ctx context.Context, jobID string) error

type startLauncher func(ctx context.Context, params startParams) error

type startParams struct {
	JobID            string
	JobTitle         string
	Queries          []string
	Geo              string
	Radius           float64
	Depth            int
	ConcurrencyMode  string
	ConcurrencyValue int
	Reviews          int
	Limit            int
	Lang             string
	Email            bool
	QueueWaitMinutes int
	OutputMode       string // "database" or "file"
	DSN              string // only from env
	JSONOut          bool
	OutDir           string
}

type templateParams struct {
	JobTitle         string   `json:"JobTitle,omitempty"`
	Queries          []string `json:"Queries"`
	Geo              string   `json:"Geo,omitempty"`
	Radius           *float64 `json:"Radius,omitempty"`
	Depth            *int     `json:"Depth,omitempty"`
	ConcurrencyMode  string   `json:"ConcurrencyMode,omitempty"`
	ConcurrencyValue *int     `json:"ConcurrencyValue,omitempty"`
	Reviews          *int     `json:"Reviews,omitempty"`
	Limit            *int     `json:"Limit,omitempty"`
	Lang             string   `json:"Lang,omitempty"`
	Email            bool     `json:"Email,omitempty"`
	QueueWaitMinutes *int     `json:"QueueWaitMinutes,omitempty"`
	OutputMode       string   `json:"OutputMode,omitempty"`
	JSONOut          bool     `json:"JSONOut,omitempty"`
	OutDir           string   `json:"OutDir,omitempty"`
}

func defaultControlOutDir(stateDB string) string {
	dir := filepath.Dir(stateDB)
	if dir == "." || dir == "" {
		return "gmdata/output"
	}
	return filepath.Join(dir, "output")
}

type controlPageData struct {
	Jobs      []jobView
	Templates []gmaps.JobTemplate
}

type jobView struct {
	ID             string
	StatusLabel    string
	StatusClass    string
	StatusHelp     string
	Progress       string
	LastError      string
	ShowPause      bool
	ShowResume     bool
	RawStatus      string
	PauseRequested bool
}

func startControlServer(ctx context.Context, addr string, store *gmaps.JobStore, stateDB string, launchResume resumeLauncher, launchStart startLauncher) (*http.Server, error) {
	username := os.Getenv("CONTROL_USERNAME")
	password := os.Getenv("CONTROL_PASSWORD")

	mux := http.NewServeMux()
	registerControlHandlers(mux, store, stateDB, launchResume, launchStart)
	if launchStart != nil {
		go runJobQueue(ctx, store, stateDB, launchStart, 30*time.Second)
	}

	var handler http.Handler = mux
	if username == "" || password == "" {
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "control UI is not configured: CONTROL_USERNAME and CONTROL_PASSWORD must both be set", http.StatusServiceUnavailable)
		})
	} else {
		handler = basicAuthMiddleware(username, password, mux)
	}

	srv := &http.Server{Addr: addr, Handler: handler}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	go func() {
		log.Printf("control UI listening on http://%s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("control server error: %v", err)
		}
	}()
	return srv, nil
}

func basicAuthMiddleware(username, password string, next http.Handler) http.Handler {
	usernameHash := sha256.Sum256([]byte(username))
	passwordHash := sha256.Sum256([]byte(password))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		uHash := sha256.Sum256([]byte(u))
		pHash := sha256.Sum256([]byte(p))
		uMatch := subtle.ConstantTimeCompare(uHash[:], usernameHash[:]) == 1
		pMatch := subtle.ConstantTimeCompare(pHash[:], passwordHash[:]) == 1
		if !ok || !uMatch || !pMatch {
			w.Header().Set("WWW-Authenticate", `Basic realm="Scraper Control"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func registerControlHandlers(mux *http.ServeMux, store *gmaps.JobStore, stateDB string, launchResume resumeLauncher, launchStart startLauncher) {
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		jobs, err := store.ListJobs(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		templates, err := store.ListJobTemplates(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = controlTemplate.Execute(w, newControlPageData(jobs, templates))
	})
	mux.HandleFunc("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			jobs, err := store.ListJobs(r.Context())
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, jobs)
			return
		}
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	})
	mux.HandleFunc("/api/job-templates", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		templates, err := store.ListJobTemplates(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, templates)
	})
	mux.HandleFunc("/api/jobs/start", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if launchStart == nil {
			http.Error(w, "start launcher is not configured", http.StatusServiceUnavailable)
			return
		}
		params, errMsg := parseStartParams(r)
		if errMsg != "" {
			http.Error(w, errMsg, http.StatusBadRequest)
			return
		}
		if params.OutputMode == "file" && params.OutDir == "" {
			params.OutDir = defaultControlOutDir(stateDB)
		}
		jobID, err := store.CreateStartingJob(r.Context(), params.Queries, scraperConfigFromStartParams(params))
		if errors.Is(err, gmaps.ErrActiveJobExists) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		params.JobID = jobID
		templateParams := templateParamsFromForm(r, params)
		if _, err := store.SaveJobTemplate(r.Context(), jobTemplateName(params), templateParams); err != nil {
			log.Printf("save job template: %v", err)
		}
		job, err := store.GetJob(r.Context(), jobID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		status := "queued"
		if job.Status == gmaps.JobStatusStarting {
			if err := launchStart(r.Context(), params); err != nil {
				_ = store.SetJobStatus(context.Background(), jobID, gmaps.JobStatusFailed, err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			status = "started"
		}
		writeJSON(w, map[string]string{"status": status, "job_id": jobID})
	})
	mux.HandleFunc("/api/jobs/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/jobs/")
		parts := strings.Split(strings.Trim(path, "/"), "/")
		if len(parts) == 0 || parts[0] == "" {
			http.NotFound(w, r)
			return
		}
		jobID := parts[0]
		if len(parts) == 1 && r.Method == http.MethodGet {
			job, err := store.GetJob(r.Context(), jobID)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			writeJSON(w, job)
			return
		}
		if len(parts) != 2 || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		switch parts[1] {
		case "pause":
			if err := store.RequestPause(r.Context(), jobID); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, map[string]string{"status": "pause_requested"})
		case "resume":
			if launchResume == nil {
				http.Error(w, "resume launcher is not configured", http.StatusServiceUnavailable)
				return
			}
			if err := launchResume(r.Context(), jobID); err != nil {
				if _, ok := err.(errHTTP); ok {
					http.Error(w, err.Error(), http.StatusConflict)
					return
				}
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, map[string]string{"status": "resume_started"})
		default:
			http.NotFound(w, r)
		}
	})
}

func newControlPageData(jobs []gmaps.Job, templates []gmaps.JobTemplate) controlPageData {
	views := make([]jobView, 0, len(jobs))
	for _, job := range jobs {
		views = append(views, newJobView(job))
	}
	return controlPageData{Jobs: views, Templates: templates}
}

func newJobView(job gmaps.Job) jobView {
	label, class, help := jobStatusDisplay(job)
	view := jobView{
		ID:             job.ID,
		StatusLabel:    label,
		StatusClass:    class,
		StatusHelp:     help,
		Progress:       formatJobProgress(job.Stats),
		RawStatus:      job.Status,
		PauseRequested: job.PauseRequested,
		ShowPause:      job.Status == gmaps.JobStatusRunning && !job.PauseRequested,
		ShowResume:     job.Status == gmaps.JobStatusPaused || job.Status == gmaps.JobStatusBlocked || job.Status == gmaps.JobStatusFailed,
	}
	if job.LastError.Valid {
		view.LastError = job.LastError.String
	}
	return view
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

func parseStartParams(r *http.Request) (startParams, string) {
	if err := r.ParseForm(); err != nil {
		return startParams{}, "invalid form data"
	}
	queriesRaw := strings.TrimSpace(r.FormValue("queries"))
	if queriesRaw == "" {
		return startParams{}, "queries is required"
	}
	var queries []string
	for _, q := range strings.Split(queriesRaw, "\n") {
		q = strings.TrimSpace(q)
		if q != "" {
			queries = append(queries, q)
		}
	}
	if len(queries) == 0 {
		return startParams{}, "queries is required"
	}

	outputMode := r.FormValue("output_mode")
	if outputMode != "database" && outputMode != "file" {
		outputMode = "file"
	}

	var dsn string
	if outputMode == "database" {
		dsn = os.Getenv("DSN")
		if dsn == "" {
			return startParams{}, "database output mode requires DSN environment variable to be set"
		}
	}

	p := startParams{
		JobTitle:         strings.TrimSpace(r.FormValue("job_title")),
		Queries:          queries,
		Geo:              strings.TrimSpace(r.FormValue("geo")),
		Lang:             strings.TrimSpace(r.FormValue("lang")),
		ConcurrencyMode:  strings.TrimSpace(r.FormValue("concurrency_mode")),
		QueueWaitMinutes: 20,
		Email:            r.FormValue("email") == "1" || r.FormValue("email") == "true" || r.FormValue("email") == "on",
		OutputMode:       outputMode,
		DSN:              dsn,
		JSONOut:          true,
	}
	if p.Lang == "" {
		p.Lang = "en"
	}
	if p.ConcurrencyMode != "c" && p.ConcurrencyMode != "max-c" {
		p.ConcurrencyMode = "max-c"
	}
	if p.Geo != "" {
		if _, _, err := gmaps.ParseGeoCenter(p.Geo); err != nil {
			return startParams{}, "invalid geo center"
		}
	}

	if v := r.FormValue("radius"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 {
			return startParams{}, "radius must be a non-negative number"
		}
		p.Radius = f
	}
	if p.Radius > 0 && p.Geo == "" {
		return startParams{}, "radius requires geo center"
	}
	if v := r.FormValue("depth"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return startParams{}, "depth must be a positive integer"
		}
		p.Depth = n
	}
	if v := r.FormValue("concurrency_value"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return startParams{}, "concurrency must be a positive integer"
		}
		p.ConcurrencyValue = n
	}
	if v := r.FormValue("reviews"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return startParams{}, "reviews must be a non-negative integer"
		}
		p.Reviews = n
	}
	if v := r.FormValue("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return startParams{}, "limit must be a non-negative integer"
		}
		p.Limit = n
	}
	if v := r.FormValue("queue_wait_minutes"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return startParams{}, "queue wait must be a non-negative integer"
		}
		p.QueueWaitMinutes = n
	}

	return p, ""
}

func scraperConfigFromStartParams(p startParams) gmaps.Config {
	cfg := gmaps.Config{
		ConcurrencyMode:  p.ConcurrencyMode,
		ConcurrencyValue: p.ConcurrencyValue,
		QueueWaitMinutes: p.QueueWaitMinutes,
		Depth:            p.Depth,
		Lang:             p.Lang,
		Geo:              p.Geo,
		Radius:           p.Radius,
		ExtractEmail:     p.Email,
		ExtraReviews:     p.Reviews,
		Limit:            p.Limit,
		JobID:            p.JobID,
		OutputMode:       p.OutputMode,
		JSONOut:          p.JSONOut,
		OutDir:           p.OutDir,
	}
	switch p.ConcurrencyMode {
	case "c":
		cfg.Concurrency = p.ConcurrencyValue
	case "max-c":
		cfg.MaxConcurrency = p.ConcurrencyValue
	}
	return cfg
}

func jobTemplateName(p startParams) string {
	if p.JobTitle != "" {
		return p.JobTitle
	}
	name := "job"
	if len(p.Queries) > 0 {
		name = p.Queries[0]
	}
	if len(name) > 48 {
		name = name[:48]
	}
	return strings.TrimSpace(name + " [" + p.OutputMode + "/" + p.Lang + "]")
}

func templateParamsFromForm(r *http.Request, p startParams) templateParams {
	t := templateParams{
		JobTitle:        p.JobTitle,
		Queries:         append([]string(nil), p.Queries...),
		Geo:             p.Geo,
		ConcurrencyMode: p.ConcurrencyMode,
		Lang:            p.Lang,
		Email:           p.Email,
		OutputMode:      p.OutputMode,
		JSONOut:         p.JSONOut,
		OutDir:          p.OutDir,
	}
	if strings.TrimSpace(r.FormValue("radius")) != "" {
		t.Radius = &p.Radius
	}
	if strings.TrimSpace(r.FormValue("depth")) != "" {
		t.Depth = &p.Depth
	}
	if strings.TrimSpace(r.FormValue("concurrency_value")) != "" {
		t.ConcurrencyValue = &p.ConcurrencyValue
	}
	if strings.TrimSpace(r.FormValue("reviews")) != "" {
		t.Reviews = &p.Reviews
	}
	if strings.TrimSpace(r.FormValue("limit")) != "" {
		t.Limit = &p.Limit
	}
	if strings.TrimSpace(r.FormValue("queue_wait_minutes")) != "" {
		t.QueueWaitMinutes = &p.QueueWaitMinutes
	}
	return t
}

func startParamsFromJob(job *gmaps.Job, stateDB string) (startParams, error) {
	var cfg gmaps.Config
	if err := json.Unmarshal([]byte(job.ConfigJSON), &cfg); err != nil {
		return startParams{}, err
	}
	p := startParams{
		JobID:            job.ID,
		Queries:          job.Queries,
		Geo:              cfg.Geo,
		Radius:           cfg.Radius,
		Depth:            cfg.Depth,
		ConcurrencyMode:  cfg.ConcurrencyMode,
		ConcurrencyValue: cfg.ConcurrencyValue,
		Reviews:          cfg.ExtraReviews,
		Limit:            cfg.Limit,
		Lang:             cfg.Lang,
		Email:            cfg.ExtractEmail,
		QueueWaitMinutes: cfg.QueueWaitMinutes,
		OutputMode:       cfg.OutputMode,
		JSONOut:          cfg.JSONOut,
		OutDir:           cfg.OutDir,
	}
	if p.Lang == "" {
		p.Lang = "en"
	}
	if p.OutputMode == "" {
		p.OutputMode = "file"
	}
	if p.ConcurrencyMode == "" {
		switch {
		case cfg.MaxConcurrency > 0:
			p.ConcurrencyMode = "max-c"
			p.ConcurrencyValue = cfg.MaxConcurrency
		case cfg.Concurrency > 0:
			p.ConcurrencyMode = "c"
			p.ConcurrencyValue = cfg.Concurrency
		default:
			p.ConcurrencyMode = "max-c"
		}
	}
	if p.OutputMode == "database" {
		p.DSN = os.Getenv("DSN")
		if p.DSN == "" {
			return startParams{}, errors.New("database output mode requires DSN environment variable to be set")
		}
	} else if p.OutDir == "" {
		p.OutDir = defaultControlOutDir(stateDB)
	}
	return p, nil
}

func runJobQueue(ctx context.Context, store *gmaps.JobStore, stateDB string, launchStart startLauncher, pollInterval time.Duration) {
	if pollInterval <= 0 {
		pollInterval = 30 * time.Second
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if err := runJobQueueOnce(ctx, store, stateDB, launchStart); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("job queue: %v", err)
		}
		timer.Reset(pollInterval)
	}
}

func runJobQueueOnce(ctx context.Context, store *gmaps.JobStore, stateDB string, launchStart startLauncher) error {
	job, err := store.NextPendingJobAfterDone(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	params, err := startParamsFromJob(job, stateDB)
	if err != nil {
		return err
	}
	if params.QueueWaitMinutes > 0 {
		if err := sleepContext(ctx, time.Duration(params.QueueWaitMinutes)*time.Minute); err != nil {
			return err
		}
	}
	claimed, err := store.ClaimNextPendingJob(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	params, err = startParamsFromJob(claimed, stateDB)
	if err != nil {
		_ = store.SetJobStatus(context.Background(), claimed.ID, gmaps.JobStatusFailed, err)
		return err
	}
	if err := launchStart(ctx, params); err != nil {
		_ = store.SetJobStatus(context.Background(), claimed.ID, gmaps.JobStatusFailed, err)
		return err
	}
	return nil
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

var controlTemplate = template.Must(template.New("control").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Scraper Jobs</title>
  <style>
    body { font-family: system-ui, -apple-system, sans-serif; margin: 32px; color: #17202a; }
    h2 { margin-top: 40px; }
    table { border-collapse: collapse; width: 100%; }
    th, td { border-bottom: 1px solid #d8dee4; padding: 8px; text-align: left; }
    button { padding: 6px 10px; }
    button[disabled] { color: #98a2b3; cursor: not-allowed; }
    .muted { color: #667085; }
    .status { display: inline-flex; align-items: center; border-radius: 999px; padding: 3px 9px; font-size: 0.9rem; font-weight: 600; border: 1px solid transparent; }
    .status-starting, .status-pausing { background: #fff7ed; color: #9a3412; border-color: #fed7aa; }
    .status-running { background: #ecfdf3; color: #027a48; border-color: #abefc6; }
    .status-paused, .status-pending { background: #eff8ff; color: #175cd3; border-color: #b2ddff; }
    .status-blocked { background: #fffbeb; color: #92400e; border-color: #fde68a; }
    .status-done { background: #f0fdf4; color: #166534; border-color: #bbf7d0; }
    .status-failed { background: #fef3f2; color: #b42318; border-color: #fecdca; }
    .status-unknown { background: #f2f4f7; color: #344054; border-color: #d0d5dd; }
    .error-text { display: block; max-width: 420px; margin-top: 4px; color: #b42318; font-size: 0.88rem; overflow-wrap: anywhere; }
    .raw-status { display: block; margin-top: 4px; font-size: 0.82rem; }
    form { display: grid; grid-template-columns: max-content 1fr; gap: 8px 16px; max-width: 600px; align-items: start; }
    form label { padding-top: 6px; font-weight: 500; }
    form input, form select, form textarea { padding: 6px 8px; border: 1px solid #d8dee4; border-radius: 4px; width: 100%; box-sizing: border-box; }
    form textarea { height: 80px; resize: vertical; }
    .form-submit { grid-column: 2; margin-top: 8px; }
    .form-submit button { padding: 8px 20px; background: #2563eb; color: #fff; border: none; border-radius: 4px; cursor: pointer; }
    #start-result { margin-top: 12px; }
  </style>
</head>
<body>
  <h1>Scraper Jobs</h1>

  <h2>Create Job</h2>
  <form id="start-form">
    <label for="template_select">Template</label>
    <select id="template_select">
      <option value="">Select a previous job...</option>
    </select>

    <label for="job_title">Job title</label>
    <input id="job_title" name="job_title" type="text" placeholder="Optional template title">

    <label for="queries">Queries *</label>
    <textarea id="queries" name="queries" placeholder="One query per line&#10;e.g. restaurants in toronto"></textarea>

    <label for="output_mode">Output</label>
    <select id="output_mode" name="output_mode">
      <option value="file">File (JSON)</option>
      <option value="database">Database (DSN from env)</option>
    </select>

    <label for="geo">Geo center</label>
    <input id="geo" name="geo" type="text" placeholder="lat,lng,zoomz e.g. 43.65,-79.38,14z">

    <label for="radius">Radius (m)</label>
    <input id="radius" name="radius" type="number" min="0" step="100" placeholder="0 = no filter">

    <label for="depth">Depth</label>
    <input id="depth" name="depth" type="number" min="1" placeholder="default 10">

    <label for="concurrency_mode">Concurrency flag</label>
    <select id="concurrency_mode" name="concurrency_mode">
      <option value="max-c">Max concurrency (-max-c)</option>
      <option value="c">Concurrency (-c)</option>
    </select>

    <label for="concurrency_value">Concurrency value</label>
    <input id="concurrency_value" name="concurrency_value" type="number" min="1" placeholder="default 1">

    <label for="reviews">Min reviews</label>
    <input id="reviews" name="reviews" type="number" min="0" placeholder="0 = page default">

    <label for="limit">Limit</label>
    <input id="limit" name="limit" type="number" min="0" placeholder="0 = no limit">

    <label for="lang">Language</label>
    <input id="lang" name="lang" type="text" placeholder="en">

    <label for="email">Extract emails</label>
    <input id="email" name="email" type="checkbox" value="on" style="width:auto">

    <label for="queue_wait_minutes">Queue wait (minutes)</label>
    <input id="queue_wait_minutes" name="queue_wait_minutes" type="number" min="0" value="20">

    <div class="form-submit">
      <button type="submit">Create Job</button>
    </div>
  </form>
  <div id="start-result"></div>

  <h2>Jobs</h2>
  <table>
    <thead><tr><th>Job</th><th>Status</th><th>Progress</th><th>Actions</th></tr></thead>
    <tbody id="jobs-tbody">
    {{range .Jobs}}
      <tr data-job-id="{{.ID}}">
        <td><code>{{.ID}}</code></td>
        <td>
          <span class="status {{.StatusClass}}" title="{{.StatusHelp}}">{{.StatusLabel}}</span>
          <span class="raw-status muted" title="Raw DB state">raw: {{.RawStatus}}{{if .PauseRequested}}, pause requested{{end}}</span>
          <span class="error-text" title="{{.LastError}}"{{if not .LastError}} hidden{{end}}>{{.LastError}}</span>
        </td>
        <td class="progress">{{.Progress}}</td>
        <td class="actions">
          <button onclick="refreshJob('{{.ID}}')" title="Refresh this row from the saved job database.">Refresh</button>
          {{if .ShowPause}}
            <button class="pause-action" onclick="post('/api/jobs/{{.ID}}/pause')" title="Request a graceful pause. Active scrapes finish before the job becomes paused.">Pause</button>
          {{else}}
            <button class="pause-action" disabled title="Pause is available only while a job is running.">Pause</button>
          {{end}}
          {{if .ShowResume}}
            <button class="resume-action" onclick="post('/api/jobs/{{.ID}}/resume')" title="Start a new scraper process and continue from saved pending URLs.">Resume</button>
          {{else}}
            <button class="resume-action" disabled title="Resume is available for paused, blocked, or failed jobs.">Resume</button>
          {{end}}
        </td>
      </tr>
    {{else}}
      <tr><td colspan="4" class="muted">No jobs yet.</td></tr>
    {{end}}
    </tbody>
  </table>

  <script>
    const statusDisplay = {
      starting: ['Starting', 'status-starting', 'Collecting Google Maps result URLs before place scraping begins.'],
      running: ['Running', 'status-running', 'Scraping is active.'],
      paused: ['Paused', 'status-paused', 'Stopped safely; resume continues from saved pending URLs.'],
      blocked: ['Blocked', 'status-blocked', 'Google likely rate-limited or blocked the browser session; resume later.'],
      done: ['Done', 'status-done', 'All queued URLs were processed.'],
      failed: ['Failed', 'status-failed', 'The job stopped with an error.'],
      pending: ['Pending', 'status-pending', 'Job is created but has not started yet.']
    };

    async function post(path) {
      const res = await fetch(path, { method: 'POST' });
      if (!res.ok) {
        const msg = await res.text();
        alert('Error: ' + msg);
      }
      location.reload();
    }

    async function refreshJob(jobID) {
      const row = document.querySelector('tr[data-job-id="' + CSS.escape(jobID) + '"]');
      if (!row) return;
      const refreshButton = row.querySelector('button');
      refreshButton.disabled = true;
      try {
        const res = await fetch('/api/jobs/' + encodeURIComponent(jobID));
        if (!res.ok) {
          const msg = await res.text();
          alert('Error: ' + msg);
          return;
        }
        renderJobRow(row, await res.json());
      } finally {
        refreshButton.disabled = false;
      }
    }

    function renderJobRow(row, job) {
      const status = row.querySelector('.status');
      const raw = row.querySelector('.raw-status');
      const error = row.querySelector('.error-text');
      const progress = row.querySelector('.progress');
      const pause = row.querySelector('.pause-action');
      const resume = row.querySelector('.resume-action');
      const display = job.Status === 'running' && job.PauseRequested
        ? ['Pausing', 'status-pausing', 'Pause requested; active scrapes are finishing before the process exits.']
        : (statusDisplay[job.Status] || [job.Status || 'Unknown', 'status-unknown', job.Status ? 'Unrecognized job status.' : 'Job status is missing.']);

      status.className = 'status ' + display[1];
      status.title = display[2];
      status.textContent = display[0];
      raw.textContent = 'raw: ' + (job.Status || '') + (job.PauseRequested ? ', pause requested' : '');
      progress.textContent = formatProgress(job.Stats || {});

      const lastError = job.LastError && job.LastError.Valid ? job.LastError.String : '';
      error.textContent = lastError;
      error.title = lastError;
      error.hidden = !lastError;

      const canPause = job.Status === 'running' && !job.PauseRequested;
      pause.disabled = !canPause;
      if (canPause) {
        pause.setAttribute('onclick', "post('/api/jobs/" + escapeJSAttr(job.ID) + "/pause')");
      } else {
        pause.removeAttribute('onclick');
      }

      const canResume = ['paused', 'blocked', 'failed'].includes(job.Status);
      resume.disabled = !canResume;
      if (canResume) {
        resume.setAttribute('onclick', "post('/api/jobs/" + escapeJSAttr(job.ID) + "/resume')");
      } else {
        resume.removeAttribute('onclick');
      }
    }

    function formatProgress(stats) {
      if (!stats.Total) return 'No URLs queued yet';
      const parts = [stats.Done + ' / ' + stats.Total + ' done'];
      if (stats.Pending > 0) parts.push(stats.Pending + ' pending');
      if (stats.InProgress > 0) parts.push(stats.InProgress + ' active');
      if (stats.Failed > 0) parts.push(stats.Failed + ' failed');
      return parts.join(', ');
    }

    function escapeJSAttr(value) {
      return String(value).replace(/\\/g, '\\\\').replace(/'/g, "\\'");
    }

    async function loadTemplates() {
      const select = document.getElementById('template_select');
      try {
        const res = await fetch('/api/job-templates');
        if (!res.ok) return;
        const templates = await res.json();
        for (const tpl of templates || []) {
          const opt = document.createElement('option');
          opt.value = tpl.ParamsJSON || '';
          opt.textContent = tpl.Name || tpl.ID;
          select.appendChild(opt);
        }
      } catch (_) {}
    }

    function setValue(id, value) {
      const el = document.getElementById(id);
      if (!el) return;
      if (value === null || value === undefined) return;
      el.value = value === 0 ? '0' : (value || '');
    }

    document.getElementById('template_select').addEventListener('change', function() {
      if (!this.value) return;
      let p;
      try {
        p = JSON.parse(this.value);
      } catch (_) {
        return;
      }
      setValue('job_title', p.JobTitle);
      setValue('queries', (p.Queries || []).join('\n'));
      setValue('output_mode', p.OutputMode);
      setValue('geo', p.Geo);
      setValue('radius', p.Radius);
      setValue('depth', p.Depth);
      setValue('concurrency_mode', p.ConcurrencyMode);
      setValue('concurrency_value', p.ConcurrencyValue);
      setValue('reviews', p.Reviews);
      setValue('limit', p.Limit);
      setValue('lang', p.Lang);
      setValue('queue_wait_minutes', p.QueueWaitMinutes);
      document.getElementById('email').checked = !!p.Email;
    });

    document.getElementById('start-form').addEventListener('submit', async function(e) {
      e.preventDefault();
      const result = document.getElementById('start-result');
      result.textContent = '';
      const data = new URLSearchParams(new FormData(this));
      const res = await fetch('/api/jobs/start', { method: 'POST', body: data });
      const msg = await res.text();
      if (!res.ok) {
        result.style.color = '#c0392b';
        result.textContent = 'Error: ' + msg;
      } else {
        result.style.color = '#27ae60';
        try {
          const payload = JSON.parse(msg);
          result.textContent = payload.status === 'queued' ? 'Job queued. Refreshing...' : 'Job started. Refreshing...';
        } catch (_) {
          result.textContent = 'Job created. Refreshing...';
        }
        setTimeout(() => location.reload(), 1500);
      }
    });
    loadTemplates();
  </script>
</body>
</html>`))

func newProcessResumeLauncher(store *gmaps.JobStore, stateDB string) resumeLauncher {
	return func(ctx context.Context, jobID string) error {
		job, err := store.ClaimResume(ctx, jobID)
		if errors.Is(err, gmaps.ErrJobNotResumable) {
			return errHTTP(err.Error())
		}
		if err != nil {
			return err
		}
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		args, err := buildResumeArgs(job, stateDB)
		if err != nil {
			_ = store.SetJobStatus(context.Background(), jobID, gmaps.JobStatusFailed, err)
			return err
		}
		return spawnProcess(exe, args)
	}
}

func newProcessStartLauncher(stateDB string) startLauncher {
	return func(ctx context.Context, p startParams) error {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		args := buildStartArgs(p, stateDB)
		return spawnProcess(exe, args)
	}
}

func spawnProcess(exe string, args []string) error {
	cmd := exec.Command(exe, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = nil
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		return err
	}
	log.Printf("started process: %s %s (pid %d)", exe, strings.Join(args, " "), cmd.Process.Pid)
	go func() {
		if err := cmd.Wait(); err != nil {
			log.Printf("process pid %d exited: %v", cmd.Process.Pid, err)
		}
	}()
	return nil
}

func buildResumeArgs(job *gmaps.Job, stateDB string) ([]string, error) {
	args := []string{"-job", job.ID, "-state-db", stateDB}
	var cfg gmaps.Config
	if err := json.Unmarshal([]byte(job.ConfigJSON), &cfg); err != nil {
		return args, nil
	}
	switch {
	case cfg.ConcurrencyMode == "max-c" && cfg.ConcurrencyValue > 0:
		args = append(args, "-max-c", strconv.Itoa(cfg.ConcurrencyValue))
	case cfg.ConcurrencyMode == "c" && cfg.ConcurrencyValue > 0:
		args = append(args, "-c", strconv.Itoa(cfg.ConcurrencyValue))
	case cfg.MaxConcurrency > 0:
		args = append(args, "-max-c", strconv.Itoa(cfg.MaxConcurrency))
	case cfg.Concurrency > 0:
		args = append(args, "-c", strconv.Itoa(cfg.Concurrency))
	}
	if cfg.Depth > 0 {
		args = append(args, "-depth", strconv.Itoa(cfg.Depth))
	}
	if cfg.Lang != "" {
		args = append(args, "-lang", cfg.Lang)
	}
	if cfg.Geo != "" {
		args = append(args, "-geo", cfg.Geo)
	}
	if cfg.Radius > 0 {
		args = append(args, "-radius", strconv.FormatFloat(cfg.Radius, 'f', -1, 64))
	}
	if cfg.ExtractEmail {
		args = append(args, "-email")
	}
	if cfg.ExtraReviews > 0 {
		args = append(args, "-reviews", strconv.Itoa(cfg.ExtraReviews))
	}
	switch cfg.OutputMode {
	case "database":
		dsn := os.Getenv("DSN")
		if dsn == "" {
			return nil, errors.New("database output mode requires DSN environment variable to be set")
		}
		args = append(args, "-dsn", dsn)
	case "file":
		if cfg.JSONOut {
			args = append(args, "-json")
		}
		if cfg.OutDir != "" {
			args = append(args, "-o", cfg.OutDir)
		}
	default:
		if dsn := os.Getenv("DSN"); dsn != "" {
			args = append(args, "-dsn", dsn)
		}
	}
	return args, nil
}

func buildStartArgs(p startParams, stateDB string) []string {
	args := []string{
		"-job", p.JobID,
		"-state-db", stateDB,
	}
	if p.OutputMode == "database" {
		args = append(args, "-dsn", p.DSN)
	} else {
		outDir := p.OutDir
		if outDir == "" {
			outDir = defaultControlOutDir(stateDB)
		}
		args = append(args, "-json", "-o", outDir)
	}
	if p.Geo != "" {
		args = append(args, "-geo", p.Geo)
	}
	if p.Radius > 0 {
		args = append(args, "-radius", strconv.FormatFloat(p.Radius, 'f', -1, 64))
	}
	if p.Depth > 0 {
		args = append(args, "-depth", strconv.Itoa(p.Depth))
	}
	if p.ConcurrencyValue > 0 {
		switch p.ConcurrencyMode {
		case "c":
			args = append(args, "-c", strconv.Itoa(p.ConcurrencyValue))
		default:
			args = append(args, "-max-c", strconv.Itoa(p.ConcurrencyValue))
		}
	}
	if p.Reviews > 0 {
		args = append(args, "-reviews", strconv.Itoa(p.Reviews))
	}
	if p.Limit > 0 {
		args = append(args, "-limit", strconv.Itoa(p.Limit))
	}
	if p.Lang != "" {
		args = append(args, "-lang", p.Lang)
	}
	if p.Email {
		args = append(args, "-email")
	}
	return args
}

type errHTTP string

func (e errHTTP) Error() string { return string(e) }
