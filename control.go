package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
