package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gosom/google-maps-scraper-lite/gmaps"
)

var noopStartLauncher startLauncher = func(_ context.Context, _ startParams) error { return nil }

func TestControlPauseEndpoint(t *testing.T) {
	store, err := gmaps.OpenJobStore(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	jobID, err := store.CreateJob(context.Background(), []string{"coffee"}, nil, []string{"u1"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/"+jobID+"/pause", nil)
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	registerControlHandlers(mux, store, "gmdata/scraper-state.sqlite", func(ctx context.Context, jobID string) error { return nil }, noopStartLauncher)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	paused, err := store.PauseRequested(context.Background(), jobID)
	if err != nil {
		t.Fatalf("pause requested: %v", err)
	}
	if !paused {
		t.Fatal("pause flag was not set")
	}
}

func TestControlIndexListsJobs(t *testing.T) {
	store, err := gmaps.OpenJobStore(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	jobID, err := store.CreateJob(context.Background(), []string{"coffee"}, nil, []string{"u1"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	registerControlHandlers(mux, store, "gmdata/scraper-state.sqlite", func(ctx context.Context, jobID string) error { return nil }, noopStartLauncher)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), jobID) {
		t.Fatalf("index did not include job ID %s: %s", jobID, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `data-job-id="`+jobID+`"`) {
		t.Fatalf("index did not include refreshable row for job ID %s: %s", jobID, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `refreshJob('`+jobID+`')`) {
		t.Fatalf("index did not include refresh button for job ID %s: %s", jobID, rec.Body.String())
	}
}

func TestControlJobEndpointReturnsCurrentJob(t *testing.T) {
	store, err := gmaps.OpenJobStore(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	jobID, err := store.CreateJob(context.Background(), []string{"coffee"}, nil, []string{"u1"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := store.StartJob(context.Background(), jobID); err != nil {
		t.Fatalf("start job: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/jobs/"+jobID, nil)
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	registerControlHandlers(mux, store, "gmdata/scraper-state.sqlite", func(ctx context.Context, jobID string) error { return nil }, noopStartLauncher)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var job gmaps.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
		t.Fatalf("decode job: %v", err)
	}
	if job.ID != jobID {
		t.Fatalf("job ID = %q, want %q", job.ID, jobID)
	}
	if job.Status != gmaps.JobStatusRunning {
		t.Fatalf("job status = %q, want %q", job.Status, gmaps.JobStatusRunning)
	}
}

func TestJobViewShowsPausingForRunningPauseRequested(t *testing.T) {
	view := newJobView(gmaps.Job{
		Status:         gmaps.JobStatusRunning,
		PauseRequested: true,
		Stats:          gmaps.JobStats{Total: 3, Done: 1, Pending: 1, InProgress: 1},
	})
	if view.StatusLabel != "Pausing" {
		t.Fatalf("StatusLabel = %q, want Pausing", view.StatusLabel)
	}
	if view.ShowPause {
		t.Fatal("pause action should be hidden while pause is already requested")
	}
	if view.Progress != "1 / 3 done, 1 pending, 1 active" {
		t.Fatalf("Progress = %q", view.Progress)
	}
}

func TestJobViewActionsForPausedJob(t *testing.T) {
	view := newJobView(gmaps.Job{Status: gmaps.JobStatusPaused})
	if view.StatusLabel != "Paused" {
		t.Fatalf("StatusLabel = %q, want Paused", view.StatusLabel)
	}
	if !view.ShowResume {
		t.Fatal("paused job should show resume")
	}
	if view.ShowPause {
		t.Fatal("paused job should not show pause")
	}
}

func TestControlResumeEndpointLaunchesJob(t *testing.T) {
	store, err := gmaps.OpenJobStore(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	jobID, err := store.CreateJob(context.Background(), []string{"coffee"}, nil, []string{"u1"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := store.RequestPause(context.Background(), jobID); err != nil {
		t.Fatalf("pause job: %v", err)
	}

	var launched string
	req := httptest.NewRequest(http.MethodPost, "/api/jobs/"+jobID+"/resume", nil)
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	registerControlHandlers(mux, store, "gmdata/scraper-state.sqlite", func(ctx context.Context, id string) error {
		launched = id
		return store.ClearPause(ctx, id)
	}, noopStartLauncher)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if launched != jobID {
		t.Fatalf("launched job = %q, want %q", launched, jobID)
	}
	paused, err := store.PauseRequested(context.Background(), jobID)
	if err != nil {
		t.Fatalf("pause requested: %v", err)
	}
	if paused {
		t.Fatal("resume did not clear pause flag")
	}
}

func TestControlResumeEndpointConflict(t *testing.T) {
	store, err := gmaps.OpenJobStore(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	jobID, err := store.CreateJob(context.Background(), []string{"coffee"}, nil, []string{"u1"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/"+jobID+"/resume", nil)
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	registerControlHandlers(mux, store, "gmdata/scraper-state.sqlite", func(ctx context.Context, id string) error {
		return errHTTP("job is already running or done")
	}, noopStartLauncher)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
}

// ── Basic Auth middleware tests ───────────────────────────────────────────────

func TestBasicAuthNoCreds(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := basicAuthMiddleware("user", "pass", inner)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
}

func TestBasicAuthWrongCreds(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := basicAuthMiddleware("user", "pass", inner)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("user:wrong")))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
}

func TestBasicAuthCorrectCreds(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := basicAuthMiddleware("user", "pass", inner)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("user:pass")))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
}

// ── Start job endpoint tests ──────────────────────────────────────────────────

func newStartStore(t *testing.T) *gmaps.JobStore {
	t.Helper()
	store, err := gmaps.OpenJobStore(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func postStart(mux *http.ServeMux, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/jobs/start", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestStartJobMissingQueries(t *testing.T) {
	store := newStartStore(t)
	mux := http.NewServeMux()
	registerControlHandlers(mux, store, "gmdata/scraper-state.sqlite", nil, noopStartLauncher)
	rec := postStart(mux, url.Values{"output_mode": {"file"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rec.Code, rec.Body)
	}
}

func TestStartJobDatabaseNoDSN(t *testing.T) {
	t.Setenv("DSN", "")
	store := newStartStore(t)
	mux := http.NewServeMux()
	registerControlHandlers(mux, store, "gmdata/scraper-state.sqlite", nil, noopStartLauncher)
	rec := postStart(mux, url.Values{"queries": {"coffee"}, "output_mode": {"database"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rec.Code, rec.Body)
	}
}

func TestStartJobDatabaseWithDSN(t *testing.T) {
	t.Setenv("DSN", "postgres://localhost/test")
	store := newStartStore(t)
	var captured startParams
	launcher := startLauncher(func(_ context.Context, p startParams) error {
		captured = p
		return nil
	})
	mux := http.NewServeMux()
	registerControlHandlers(mux, store, "gmdata/scraper-state.sqlite", nil, launcher)
	rec := postStart(mux, url.Values{"queries": {"coffee"}, "output_mode": {"database"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body)
	}
	if captured.DSN != "postgres://localhost/test" {
		t.Fatalf("DSN = %q, want postgres://localhost/test", captured.DSN)
	}
	if captured.OutputMode != "database" {
		t.Fatalf("OutputMode = %q, want database", captured.OutputMode)
	}
	if captured.JobID == "" {
		t.Fatal("JobID was not assigned before launch")
	}
	job, err := store.GetJob(context.Background(), captured.JobID)
	if err != nil {
		t.Fatalf("load starting job: %v", err)
	}
	if job.Status != gmaps.JobStatusStarting {
		t.Fatalf("job status = %q, want starting", job.Status)
	}
}

func TestStartJobFileMode(t *testing.T) {
	store := newStartStore(t)
	var captured startParams
	launcher := startLauncher(func(_ context.Context, p startParams) error {
		captured = p
		return nil
	})
	mux := http.NewServeMux()
	registerControlHandlers(mux, store, "gmdata/scraper-state.sqlite", nil, launcher)
	rec := postStart(mux, url.Values{"queries": {"pizza"}, "output_mode": {"file"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body)
	}
	if captured.OutputMode != "file" {
		t.Fatalf("OutputMode = %q, want file", captured.OutputMode)
	}
	if len(captured.Queries) != 1 || captured.Queries[0] != "pizza" {
		t.Fatalf("Queries = %v, want [pizza]", captured.Queries)
	}
	if captured.OutDir != filepath.Join("gmdata", "output") {
		t.Fatalf("OutDir = %q, want %q", captured.OutDir, filepath.Join("gmdata", "output"))
	}
}

func TestStartJobQueuesWhenRunning(t *testing.T) {
	store := newStartStore(t)
	jobID, err := store.CreateJob(context.Background(), []string{"coffee"}, nil, []string{"u1"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := store.StartJob(context.Background(), jobID); err != nil {
		t.Fatalf("start job: %v", err)
	}
	mux := http.NewServeMux()
	registerControlHandlers(mux, store, "gmdata/scraper-state.sqlite", nil, noopStartLauncher)
	rec := postStart(mux, url.Values{"queries": {"tea"}, "output_mode": {"file"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "queued") {
		t.Fatalf("response should contain queued, got: %s", rec.Body.String())
	}
}

func TestStartJobQueuesWhenStarting(t *testing.T) {
	store := newStartStore(t)
	if _, err := store.CreateStartingJob(context.Background(), []string{"coffee"}, nil); err != nil {
		t.Fatalf("create starting job: %v", err)
	}
	mux := http.NewServeMux()
	registerControlHandlers(mux, store, "gmdata/scraper-state.sqlite", nil, noopStartLauncher)
	rec := postStart(mux, url.Values{"queries": {"tea"}, "output_mode": {"file"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "queued") {
		t.Fatalf("response should contain queued, got: %s", rec.Body.String())
	}
}

func TestStartJobReturnsImmediately(t *testing.T) {
	store := newStartStore(t)
	called := false
	launcher := startLauncher(func(_ context.Context, _ startParams) error {
		called = true
		return nil
	})
	mux := http.NewServeMux()
	registerControlHandlers(mux, store, "gmdata/scraper-state.sqlite", nil, launcher)
	rec := postStart(mux, url.Values{"queries": {"tea"}, "output_mode": {"file"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body)
	}
	if !called {
		t.Fatal("launcher was not called")
	}
	body := rec.Body.String()
	if !strings.Contains(body, "started") {
		t.Fatalf("response should contain 'started', got: %s", body)
	}
}

func TestStartJobTemplateOmitsBlankNumericFields(t *testing.T) {
	store := newStartStore(t)
	mux := http.NewServeMux()
	registerControlHandlers(mux, store, "gmdata/scraper-state.sqlite", nil, noopStartLauncher)
	rec := postStart(mux, url.Values{
		"queries":            {"tea"},
		"output_mode":        {"file"},
		"queue_wait_minutes": {"0"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body)
	}
	templates, err := store.ListJobTemplates(context.Background())
	if err != nil {
		t.Fatalf("list templates: %v", err)
	}
	if len(templates) != 1 {
		t.Fatalf("templates len = %d, want 1", len(templates))
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(templates[0].ParamsJSON), &payload); err != nil {
		t.Fatalf("decode template params: %v", err)
	}
	if _, ok := payload["Depth"]; ok {
		t.Fatalf("Depth should be omitted for blank input: %s", templates[0].ParamsJSON)
	}
	if _, ok := payload["ConcurrencyValue"]; ok {
		t.Fatalf("ConcurrencyValue should be omitted for blank input: %s", templates[0].ParamsJSON)
	}
	if got, ok := payload["QueueWaitMinutes"].(float64); !ok || got != 0 {
		t.Fatalf("QueueWaitMinutes = %#v, want explicit 0", payload["QueueWaitMinutes"])
	}
}

func TestStartJobTemplateUsesCustomJobTitle(t *testing.T) {
	store := newStartStore(t)
	mux := http.NewServeMux()
	registerControlHandlers(mux, store, "gmdata/scraper-state.sqlite", nil, noopStartLauncher)
	rec := postStart(mux, url.Values{
		"job_title":   {"My custom scrape"},
		"queries":     {"tea"},
		"output_mode": {"file"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body)
	}
	templates, err := store.ListJobTemplates(context.Background())
	if err != nil {
		t.Fatalf("list templates: %v", err)
	}
	if len(templates) != 1 {
		t.Fatalf("templates len = %d, want 1", len(templates))
	}
	if templates[0].Name != "My custom scrape" {
		t.Fatalf("template name = %q, want custom title", templates[0].Name)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(templates[0].ParamsJSON), &payload); err != nil {
		t.Fatalf("decode template params: %v", err)
	}
	if payload["JobTitle"] != "My custom scrape" {
		t.Fatalf("JobTitle = %#v, want custom title", payload["JobTitle"])
	}
}

func TestStartJobTemplateFallsBackToGeneratedNameForBlankJobTitle(t *testing.T) {
	store := newStartStore(t)
	mux := http.NewServeMux()
	registerControlHandlers(mux, store, "gmdata/scraper-state.sqlite", nil, noopStartLauncher)
	rec := postStart(mux, url.Values{
		"job_title":   {"   "},
		"queries":     {"tea"},
		"output_mode": {"file"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body)
	}
	templates, err := store.ListJobTemplates(context.Background())
	if err != nil {
		t.Fatalf("list templates: %v", err)
	}
	if len(templates) != 1 {
		t.Fatalf("templates len = %d, want 1", len(templates))
	}
	if templates[0].Name != "tea [file/en]" {
		t.Fatalf("template name = %q, want generated name", templates[0].Name)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(templates[0].ParamsJSON), &payload); err != nil {
		t.Fatalf("decode template params: %v", err)
	}
	if _, ok := payload["JobTitle"]; ok {
		t.Fatalf("JobTitle should be omitted for blank input: %s", templates[0].ParamsJSON)
	}
}

func TestStartJobRejectsInvalidGeo(t *testing.T) {
	store := newStartStore(t)
	mux := http.NewServeMux()
	registerControlHandlers(mux, store, "gmdata/scraper-state.sqlite", nil, noopStartLauncher)
	rec := postStart(mux, url.Values{"queries": {"tea"}, "output_mode": {"file"}, "geo": {"bad"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rec.Code, rec.Body)
	}
}

func TestStartJobRejectsRadiusWithoutGeo(t *testing.T) {
	store := newStartStore(t)
	mux := http.NewServeMux()
	registerControlHandlers(mux, store, "gmdata/scraper-state.sqlite", nil, noopStartLauncher)
	rec := postStart(mux, url.Values{"queries": {"tea"}, "output_mode": {"file"}, "radius": {"100"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rec.Code, rec.Body)
	}
}

func TestStartJobRejectsInvalidNumbers(t *testing.T) {
	store := newStartStore(t)
	mux := http.NewServeMux()
	registerControlHandlers(mux, store, "gmdata/scraper-state.sqlite", nil, noopStartLauncher)
	rec := postStart(mux, url.Values{"queries": {"tea"}, "output_mode": {"file"}, "depth": {"abc"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rec.Code, rec.Body)
	}
}

func TestBuildStartArgsUsesStartingJobID(t *testing.T) {
	args := buildStartArgs(startParams{
		JobID:      "job_1",
		Queries:    []string{"coffee"},
		OutputMode: "file",
		JSONOut:    true,
	}, filepath.Join("gmdata", "state.sqlite"))
	want := []string{"-job", "job_1", "-state-db", filepath.Join("gmdata", "state.sqlite"), "-json", "-o", filepath.Join("gmdata", "output")}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %#v, want %#v", args, want)
	}
}

func TestBuildStartArgsUsesSelectedConcurrencyFlag(t *testing.T) {
	tests := []struct {
		name string
		mode string
		want []string
	}{
		{name: "max", mode: "max-c", want: []string{"-job", "job_1", "-state-db", "state.sqlite", "-json", "-o", "gmdata/output", "-max-c", "4"}},
		{name: "regular", mode: "c", want: []string{"-job", "job_1", "-state-db", "state.sqlite", "-json", "-o", "gmdata/output", "-c", "4"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := buildStartArgs(startParams{
				JobID:            "job_1",
				OutputMode:       "file",
				ConcurrencyMode:  tt.mode,
				ConcurrencyValue: 4,
			}, "state.sqlite")
			if !reflect.DeepEqual(args, tt.want) {
				t.Fatalf("args = %#v, want %#v", args, tt.want)
			}
		})
	}
}

func TestRunJobQueueOnceLaunchesAfterDone(t *testing.T) {
	store := newStartStore(t)
	ctx := context.Background()
	firstID, err := store.CreateStartingJob(ctx, []string{"coffee"}, gmaps.Config{OutputMode: "file"})
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	secondID, err := store.CreateStartingJob(ctx, []string{"tea"}, gmaps.Config{
		OutputMode:       "file",
		JSONOut:          true,
		QueueWaitMinutes: 0,
		Lang:             "en",
	})
	if err != nil {
		t.Fatalf("create second: %v", err)
	}
	if err := store.SetJobStatus(ctx, firstID, gmaps.JobStatusDone, nil); err != nil {
		t.Fatalf("set done: %v", err)
	}
	var launched string
	launcher := startLauncher(func(_ context.Context, p startParams) error {
		launched = p.JobID
		return nil
	})
	if err := runJobQueueOnce(ctx, store, "gmdata/scraper-state.sqlite", launcher); err != nil {
		t.Fatalf("run queue: %v", err)
	}
	if launched != secondID {
		t.Fatalf("launched = %q, want %q", launched, secondID)
	}
	job, err := store.GetJob(ctx, secondID)
	if err != nil {
		t.Fatalf("get second: %v", err)
	}
	if job.Status != gmaps.JobStatusStarting {
		t.Fatalf("queued job status = %q, want starting", job.Status)
	}
}

func TestRunJobQueueOnceDoesNotLaunchAfterFailed(t *testing.T) {
	store := newStartStore(t)
	ctx := context.Background()
	firstID, err := store.CreateStartingJob(ctx, []string{"coffee"}, gmaps.Config{OutputMode: "file"})
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	if _, err := store.CreateStartingJob(ctx, []string{"tea"}, gmaps.Config{OutputMode: "file"}); err != nil {
		t.Fatalf("create second: %v", err)
	}
	if err := store.SetJobStatus(ctx, firstID, gmaps.JobStatusFailed, nil); err != nil {
		t.Fatalf("set failed: %v", err)
	}
	called := false
	launcher := startLauncher(func(_ context.Context, _ startParams) error {
		called = true
		return nil
	})
	if err := runJobQueueOnce(ctx, store, "gmdata/scraper-state.sqlite", launcher); err != nil {
		t.Fatalf("run queue: %v", err)
	}
	if called {
		t.Fatal("queue launched after failed job")
	}
}

func TestDefaultControlOutDirFollowsStateDB(t *testing.T) {
	tests := []struct {
		name    string
		stateDB string
		want    string
	}{
		{
			name:    "local relative",
			stateDB: filepath.Join("gmdata", "scraper-state.sqlite"),
			want:    filepath.Join("gmdata", "output"),
		},
		{
			name:    "docker absolute",
			stateDB: filepath.Join(string(filepath.Separator), "data", "gmdata", "scraper-state.sqlite"),
			want:    filepath.Join(string(filepath.Separator), "data", "gmdata", "output"),
		},
		{
			name:    "bare filename",
			stateDB: "scraper-state.sqlite",
			want:    filepath.Join("gmdata", "output"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := defaultControlOutDir(tt.stateDB); got != tt.want {
				t.Fatalf("defaultControlOutDir(%q) = %q, want %q", tt.stateDB, got, tt.want)
			}
		})
	}
}

func TestBuildResumeArgsFileMode(t *testing.T) {
	job := &gmaps.Job{
		ID:         "job_file",
		ConfigJSON: `{"OutputMode":"file","JSONOut":true,"OutDir":"/data/out","Concurrency":2,"Depth":5}`,
	}
	args, err := buildResumeArgs(job, "/data/state.sqlite")
	if err != nil {
		t.Fatalf("build resume args: %v", err)
	}
	want := []string{"-job", "job_file", "-state-db", "/data/state.sqlite", "-c", "2", "-depth", "5", "-json", "-o", "/data/out"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %#v, want %#v", args, want)
	}
}

func TestBuildResumeArgsMaxConcurrencyMode(t *testing.T) {
	job := &gmaps.Job{
		ID:         "job_file",
		ConfigJSON: `{"OutputMode":"file","JSONOut":true,"OutDir":"/data/out","ConcurrencyMode":"max-c","ConcurrencyValue":3}`,
	}
	args, err := buildResumeArgs(job, "/data/state.sqlite")
	if err != nil {
		t.Fatalf("build resume args: %v", err)
	}
	want := []string{"-job", "job_file", "-state-db", "/data/state.sqlite", "-max-c", "3", "-json", "-o", "/data/out"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %#v, want %#v", args, want)
	}
}

func TestBuildResumeArgsDatabaseMode(t *testing.T) {
	t.Setenv("DSN", "postgres://localhost/test")
	job := &gmaps.Job{
		ID:         "job_db",
		ConfigJSON: `{"OutputMode":"database"}`,
	}
	args, err := buildResumeArgs(job, "/data/state.sqlite")
	if err != nil {
		t.Fatalf("build resume args: %v", err)
	}
	want := []string{"-job", "job_db", "-state-db", "/data/state.sqlite", "-dsn", "postgres://localhost/test"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %#v, want %#v", args, want)
	}
}

func TestBuildResumeArgsDatabaseModeRequiresDSN(t *testing.T) {
	t.Setenv("DSN", "")
	job := &gmaps.Job{
		ID:         "job_db",
		ConfigJSON: `{"OutputMode":"database"}`,
	}
	if _, err := buildResumeArgs(job, "/data/state.sqlite"); err == nil {
		t.Fatal("expected missing DSN error")
	}
}

func TestLanguagePostgresTables(t *testing.T) {
	restaurant, review := languagePostgresTables("zh-TW")
	if restaurant != "restaurants_zh_tw" || review != "restaurant_reviews_zh_tw" {
		t.Fatalf("tables = %q/%q, want restaurants_zh_tw/restaurant_reviews_zh_tw", restaurant, review)
	}
}
