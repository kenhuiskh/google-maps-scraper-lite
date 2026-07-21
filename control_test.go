package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gosom/google-maps-scraper-lite/gmaps"
)

var noopStartLauncher startLauncher = func(_ context.Context, _ startParams) error { return nil }

func TestControlPauseEndpoint(t *testing.T) {
	store, err := gmaps.OpenJobStore(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	jobID, err := store.CreateJob(context.Background(), []string{"coffee"}, nil, gmaps.URLsNoLang([]string{"u1"}))
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

func TestControlStartPendingEndpoint(t *testing.T) {
	store, err := gmaps.OpenJobStore(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	activeID, err := store.CreateStartingJob(ctx, []string{"active"}, gmaps.Config{OutputMode: "file"})
	if err != nil {
		t.Fatalf("create active job: %v", err)
	}
	if err := store.StartJob(ctx, activeID); err != nil {
		t.Fatalf("start active job: %v", err)
	}
	pendingID, err := store.CreateStartingJob(ctx, []string{"pending"}, gmaps.Config{OutputMode: "file"})
	if err != nil {
		t.Fatalf("create pending job: %v", err)
	}
	var captured startParams
	launcher := startLauncher(func(_ context.Context, p startParams) error {
		captured = p
		return nil
	})
	mux := http.NewServeMux()
	registerControlHandlers(mux, store, "gmdata/scraper-state.sqlite", nil, launcher)

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/"+pendingID+"/start-pending", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("active conflict status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if err := store.SetJobStatus(ctx, activeID, gmaps.JobStatusDone, nil); err != nil {
		t.Fatalf("mark active done: %v", err)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if captured.JobID != pendingID || len(captured.Queries) != 1 || captured.Queries[0] != "pending" {
		t.Fatalf("captured params = %+v, want pending job launch", captured)
	}
	job, err := store.GetJob(ctx, pendingID)
	if err != nil {
		t.Fatalf("get pending job: %v", err)
	}
	if job.Status != gmaps.JobStatusStarting {
		t.Fatalf("pending job status = %s, want starting", job.Status)
	}
}

func TestControlRecoverStaleEndpoint(t *testing.T) {
	store, err := gmaps.OpenJobStore(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	jobID, err := store.CreateJob(ctx, []string{"coffee"}, nil, gmaps.URLsNoLang([]string{"u1"}))
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := store.StartJob(ctx, jobID); err != nil {
		t.Fatalf("start job: %v", err)
	}
	if _, err := store.ClaimNextURL(ctx, jobID); err != nil {
		t.Fatalf("claim url: %v", err)
	}
	mux := http.NewServeMux()
	registerControlHandlers(mux, store, "gmdata/scraper-state.sqlite", nil, noopStartLauncher)
	req := httptest.NewRequest(http.MethodPost, "/api/jobs/"+jobID+"/recover-stale", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("get recovered job: %v", err)
	}
	if job.Status != gmaps.JobStatusPaused || !job.LastError.Valid {
		t.Fatalf("recovered job = %s/%v, want paused with error", job.Status, job.LastError)
	}
	stats, err := store.JobStats(ctx, jobID)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.InProgress != 0 || stats.Pending != 1 {
		t.Fatalf("stats after recovery = %+v, want URL requeued", stats)
	}
}

func TestControlIndexListsJobs(t *testing.T) {
	store, err := gmaps.OpenJobStore(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	jobID, err := store.CreateJob(context.Background(), []string{"coffee"}, nil, gmaps.URLsNoLang([]string{"u1"}))
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
	if !strings.Contains(rec.Body.String(), "Scraper Control") {
		t.Fatalf("index did not include admin shell title: %s", rec.Body.String())
	}
	for _, want := range []string{"Feed URLs", "Queued", "Scraped", "Errors"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("index did not include analytics card %q: %s", want, rec.Body.String())
		}
	}
	for _, notWant := range []string{"Pending jobs", "Pending URLs", "Needs attention"} {
		if strings.Contains(rec.Body.String(), notWant) {
			t.Fatalf("index should not include old summary card %q: %s", notWant, rec.Body.String())
		}
	}
	if strings.Contains(rec.Body.String(), `id="active-log-panel"`) {
		t.Fatalf("index should not include global active log panel: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `data-log-row="`+jobID+`"`) {
		t.Fatalf("index did not include inline log row for job ID %s: %s", jobID, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `data-inspector-job="`+jobID+`"`) {
		t.Fatalf("index did not include inspector card for job ID %s: %s", jobID, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `data-log-viewer="`+jobID+`"`) {
		t.Fatalf("index did not include inspector log viewer for job ID %s: %s", jobID, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `id="mobile-log-reader"`) || !strings.Contains(rec.Body.String(), `id="mobile-log-viewer"`) {
		t.Fatalf("index did not include dedicated mobile log reader: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `data-detail-tab="overview"`) || !strings.Contains(rec.Body.String(), `data-detail-tab="metadata"`) {
		t.Fatalf("index did not include mobile details disclosure tabs: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `class="mobile-row-more"`) {
		t.Fatalf("index did not include mobile row secondary action menu: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `class="mobile-diagnostics-toggle"`) {
		t.Fatalf("index did not include mobile diagnostics disclosure: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `class="inspector-log inspector-log-console"`) {
		t.Fatalf("index did not include inspector log console structure: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "inset 3px 0 0") {
		t.Fatalf("index should not use active row side-stripe styling: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `grid-template-columns: minmax(220px, 0.85fr) minmax(190px, 0.65fr) 116px;`) {
		t.Fatalf("index should reserve enough job action column width: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "overflow-x: hidden;") || !strings.Contains(rec.Body.String(), "scrollbar-gutter: stable;") {
		t.Fatalf("index should keep job log scrollbar from covering final lines: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "height: min(720px, calc(100vh - 292px));") || !strings.Contains(rec.Body.String(), "flex: 1 1 auto;") {
		t.Fatalf("index should constrain inspector card to viewport height: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "height: 100dvh;") || !strings.Contains(rec.Body.String(), "white-space: pre-wrap;") {
		t.Fatalf("index should include readable mobile log reader sizing and wrapping: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `id="tracked-status-dot"`) {
		t.Fatalf("index did not include tracked status dot: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `data-job-id="`+jobID+`"`) {
		t.Fatalf("index did not include refreshable row for job ID %s: %s", jobID, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `refreshJob('`+jobID+`')`) {
		t.Fatalf("index did not include refresh button for job ID %s: %s", jobID, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `selectJob('`+jobID+`')`) {
		t.Fatalf("index did not include more info button for job ID %s: %s", jobID, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `aria-label="More info for job `+jobID+`"`) {
		t.Fatalf("index did not include accessible more info button for job ID %s: %s", jobID, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `start-pending`) {
		t.Fatalf("index did not include start action for unblocked pending job ID %s: %s", jobID, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `id="start-form"`) {
		t.Fatalf("index should not include job creation form: %s", rec.Body.String())
	}
}

func TestControlManagementPagesRenderDedicatedTools(t *testing.T) {
	store, err := gmaps.OpenJobStore(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	templateID, err := store.SaveJobTemplateJSON(ctx, "", "coffee template", `{"Queries":["coffee"],"OutputMode":"file"}`)
	if err != nil {
		t.Fatalf("save template: %v", err)
	}
	strategyID, err := store.SaveStrategy(ctx, "", "morning sweep", "daily", []string{templateID})
	if err != nil {
		t.Fatalf("save strategy: %v", err)
	}
	mux := http.NewServeMux()
	registerControlHandlers(mux, store, "gmdata/scraper-state.sqlite", nil, noopStartLauncher)

	req := httptest.NewRequest(http.MethodGet, "/templates", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("templates status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"Job Templates", `class="nav-link active" href="/templates"`, `id="config-import-form"`, `onclick="openConfigExportModal()"`, `id="config-export-modal"`, `id="config-export-available"`, `id="config-export-search"`, `id="config-export-selected-strategies"`, `id="config-export-selected-templates"`, `id="config-export-strategy-preview"`, `id="config-export-download"`, `onclick="exportSelectedConfig()"`, `data-export-kind="strategy" data-id="` + strategyID + `"`, `data-export-kind="template" data-id="` + templateID + `"`, `data-template-id="` + templateID + `"`, `id="config-import-collision"`, `<option value="rename" selected>Rename</option>`, `href="/templates/editor"`, `href="/templates/editor?template_id=` + templateID + `"`, `href="/templates/editor?template_id=` + templateID + `&mode=copy"`, `data-template-row="` + templateID + `"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("templates page missing %q: %s", want, body)
		}
	}
	if !strings.Contains(body, `class="command-strip disclosure-strip config-transfer-disclosure"`) {
		t.Fatalf("templates page should make import/export a responsive disclosure: %s", body)
	}
	for _, notWant := range []string{`id="start-form"`, `id="template-form"`, `name="export_strategy_ids"`, `name="export_template_ids"`} {
		if strings.Contains(body, notWant) {
			t.Fatalf("templates list page should not include %q: %s", notWant, body)
		}
	}
	if strings.Contains(body, `id="jobs-panel"`) {
		t.Fatalf("templates page should not render job queue: %s", body)
	}

	req = httptest.NewRequest(http.MethodGet, "/templates/editor", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("template editor status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body = rec.Body.String()
	for _, want := range []string{"Create Job Template", `class="nav-link active" href="/templates"`, `id="start-form"`, `id="template-form"`, `id="template-edit-json"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("template editor page missing %q: %s", want, body)
		}
	}

	req = httptest.NewRequest(http.MethodGet, "/templates/editor?template_id="+templateID, nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("template editor edit status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body = rec.Body.String()
	for _, want := range []string{"Edit Job Template", `value="coffee template"`, `value="` + templateID + `"`, "  &#34;Queries&#34;: [", "    &#34;coffee&#34;", "  &#34;OutputMode&#34;: &#34;file&#34;"} {
		if !strings.Contains(body, want) {
			t.Fatalf("template editor edit page missing %q: %s", want, body)
		}
	}

	req = httptest.NewRequest(http.MethodGet, "/templates/editor?template_id="+templateID+"&mode=copy", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("template editor copy status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body = rec.Body.String()
	for _, want := range []string{"Duplicate Job Template", `value="coffee template copy"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("template editor copy page missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, `id="template-edit-id" name="id" type="hidden" value="`+templateID+`"`) {
		t.Fatalf("copy mode should not keep original template ID: %s", body)
	}

	req = httptest.NewRequest(http.MethodGet, "/strategies", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("strategies status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body = rec.Body.String()
	for _, want := range []string{"Strategy Management", `class="nav-link active" href="/strategies"`, `id="strategy-form"`, `data-strategy-row="` + strategyID + `"`, `class="strategy-template-list"`, `type="checkbox" name="template_ids"`, `onclick="openStrategyBatchTools()"`, `id="strategy-batch-modal"`, `id="strategy-batch-tab-language"`, `id="strategy-batch-tab-dedup"`, `id="strategy-batch-preview-list"`, `id="strategy-batch-empty"`, `id="bulk-dedup-select"`, `onclick="applyBulkDedup()"`, `id="strategy-run-modal"`, `id="strategy-run-template-list"`, `id="strategy-run-template-preview"`, `id="strategy-run-confirm"`, `data-idle-label="Run Strategy"`, `button-spinner`} {
		if !strings.Contains(body, want) {
			t.Fatalf("strategies page missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, `class="form-section disclosure-section advanced-tools-disclosure"`) {
		t.Fatalf("strategies page should move bulk tools out of inline disclosures: %s", body)
	}
	if strings.Contains(body, `<select id="strategy-template-ids"`) {
		t.Fatalf("strategies page should render checkbox template choices instead of multi-select: %s", body)
	}
	if strings.Contains(body, `id="jobs-panel"`) {
		t.Fatalf("strategies page should not render job queue: %s", body)
	}
}

func TestControlConfigExportEndpoint(t *testing.T) {
	store, err := gmaps.OpenJobStore(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	templateID, err := store.SaveJobTemplateJSON(ctx, "", "coffee template", `{"Queries":["coffee"]}`)
	if err != nil {
		t.Fatalf("save template: %v", err)
	}
	otherID, err := store.SaveJobTemplateJSON(ctx, "", "tea template", `{"Queries":["tea"]}`)
	if err != nil {
		t.Fatalf("save other template: %v", err)
	}
	strategyID, err := store.SaveStrategy(ctx, "", "morning", "", []string{templateID})
	if err != nil {
		t.Fatalf("save strategy: %v", err)
	}
	mux := http.NewServeMux()
	registerControlHandlers(mux, store, "gmdata/scraper-state.sqlite", nil, noopStartLauncher)

	req := httptest.NewRequest(http.MethodGet, "/api/config/export", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Disposition"); !strings.Contains(got, "attachment") || !strings.Contains(got, "scraper-config-") {
		t.Fatalf("Content-Disposition = %q, want export attachment", got)
	}
	var cfg gmaps.ReusableConfigExport
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("decode export: %v", err)
	}
	if cfg.Version != gmaps.ConfigExportVersion || len(cfg.Templates) != 2 || len(cfg.Strategies) != 1 {
		t.Fatalf("export = %#v, want version with template and strategy", cfg)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/config/export?strategy_id="+url.QueryEscape(strategyID), nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("filtered strategy status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("decode filtered strategy export: %v", err)
	}
	if len(cfg.Strategies) != 1 || len(cfg.Templates) != 1 || cfg.Templates[0].ID != templateID {
		t.Fatalf("filtered strategy export = %#v, want selected strategy and referenced template", cfg)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/config/export?template_id="+url.QueryEscape(otherID), nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("filtered template status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("decode filtered template export: %v", err)
	}
	if len(cfg.Templates) != 1 || cfg.Templates[0].ID != otherID || len(cfg.Strategies) != 0 {
		t.Fatalf("filtered template export = %#v, want selected template only", cfg)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/config/export?strategy_id=str_missing", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing strategy status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

func TestControlConfigImportEndpoint(t *testing.T) {
	store, err := gmaps.OpenJobStore(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	mux := http.NewServeMux()
	registerControlHandlers(mux, store, "gmdata/scraper-state.sqlite", nil, noopStartLauncher)

	req := httptest.NewRequest(http.MethodPost, "/api/config/import", strings.NewReader("{"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed status = %d, want 400: %s", rec.Code, rec.Body.String())
	}

	cfg := gmaps.ReusableConfigExport{
		Version: gmaps.ConfigExportVersion,
		Templates: []gmaps.ReusableConfigTemplate{{
			ID:         "tpl_import",
			Name:       "imported template",
			ParamsJSON: `{"Queries":["coffee"]}`,
		}},
		Strategies: []gmaps.ReusableConfigStrategy{{
			ID:          "str_import",
			Name:        "imported strategy",
			TemplateIDs: []string{"tpl_import"},
		}},
	}
	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/config/import?collision=rename", bytes.NewReader(body))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("import status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var summary gmaps.ConfigImportSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	if summary.Templates.Created != 1 || summary.Strategies.Created != 1 {
		t.Fatalf("summary = %#v, want created template and strategy", summary)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/config/import?collision=bogus", bytes.NewReader(body))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unsupported mode status = %d, want 400: %s", rec.Code, rec.Body.String())
	}

	cfg.Strategies[0].TemplateIDs = []string{"tpl_missing"}
	body, _ = json.Marshal(cfg)
	req = httptest.NewRequest(http.MethodPost, "/api/config/import", bytes.NewReader(body))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing reference status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

func TestControlSummaryCountsActiveAndPendingJobs(t *testing.T) {
	store, err := gmaps.OpenJobStore(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	activeID, err := store.CreateStartingJob(ctx, []string{"coffee"}, gmaps.Config{OutputMode: "file"})
	if err != nil {
		t.Fatalf("create active job: %v", err)
	}
	if err := store.QueueStartingJobURLs(ctx, activeID, gmaps.URLsNoLang([]string{"u1", "u2"})); err != nil {
		t.Fatalf("queue urls: %v", err)
	}
	if err := store.StartJob(ctx, activeID); err != nil {
		t.Fatalf("start active job: %v", err)
	}
	if _, err := store.CreateStartingJob(ctx, []string{"tea"}, gmaps.Config{OutputMode: "file"}); err != nil {
		t.Fatalf("create pending job: %v", err)
	}
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	summary := newControlSummaryView(jobs)
	if !summary.HasActiveJob || summary.ActiveJobID != activeID {
		t.Fatalf("active summary = %#v, want active job %s", summary, activeID)
	}
	if summary.ActiveJobTitle != "coffee" {
		t.Fatalf("active title = %q, want coffee", summary.ActiveJobTitle)
	}
	if summary.RunningJobs != 1 || summary.PendingJobs != 1 {
		t.Fatalf("running/pending jobs = %d/%d, want 1/1", summary.RunningJobs, summary.PendingJobs)
	}
	if summary.PendingURLs != 2 {
		t.Fatalf("pending URLs = %d, want 2", summary.PendingURLs)
	}
}

func TestControlUIPartialsRenderSummaryAndJobs(t *testing.T) {
	store, err := gmaps.OpenJobStore(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	jobID, err := store.CreateJob(context.Background(), []string{"coffee"}, nil, gmaps.URLsNoLang([]string{"u1"}))
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	mux := http.NewServeMux()
	registerControlHandlers(mux, store, "gmdata/scraper-state.sqlite", nil, noopStartLauncher)

	req := httptest.NewRequest(http.MethodGet, "/ui/summary", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("summary status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `id="summary"`) {
		t.Fatalf("summary partial missing wrapper: %s", rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/ui/jobs", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("jobs status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `data-job-id="`+jobID+`"`) {
		t.Fatalf("jobs partial missing job row: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `data-inspector-job="`+jobID+`"`) || !strings.Contains(rec.Body.String(), `data-log-viewer="`+jobID+`"`) {
		t.Fatalf("jobs partial missing inspector log details: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `data-detail-panel="overview"`) || !strings.Contains(rec.Body.String(), `data-detail-panel="metadata"`) {
		t.Fatalf("jobs partial missing disclosure panels for mobile details: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `openMobileLogReader('`+jobID+`')`) {
		t.Fatalf("jobs partial missing mobile log reader action: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `class="inspector-log inspector-log-console"`) {
		t.Fatalf("jobs partial missing inspector log console: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "pause-action") || strings.Contains(rec.Body.String(), "resume-action") {
		t.Fatalf("jobs partial should render one lifecycle action, not separate pause/resume buttons: %s", rec.Body.String())
	}
}

func TestControlJobsPartialPaginates(t *testing.T) {
	store, err := gmaps.OpenJobStore(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	var ids []string
	for i := 1; i <= 12; i++ {
		query := "query " + strconv.Itoa(i)
		id, err := store.CreateJob(ctx, []string{query}, nil, gmaps.URLsNoLang([]string{"url-" + query}))
		if err != nil {
			t.Fatalf("create %s: %v", query, err)
		}
		ids = append(ids, id)
	}
	mux := http.NewServeMux()
	registerControlHandlers(mux, store, "gmdata/scraper-state.sqlite", nil, noopStartLauncher)
	req := httptest.NewRequest(http.MethodGet, "/ui/jobs?page=2&page_size=10", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `id="jobs-panel" data-page="2" data-page-size="10"`) {
		t.Fatalf("page data missing: %s", body)
	}
	if !strings.Contains(body, "Showing 11-12 of 12") {
		t.Fatalf("pagination range missing: %s", body)
	}
	for _, id := range []string{ids[1], ids[0]} {
		if !strings.Contains(body, `data-job-id="`+id+`"`) {
			t.Fatalf("missing job %s in second page: %s", id, body)
		}
	}
	if strings.Contains(body, `data-job-id="`+ids[11]+`"`) {
		t.Fatalf("newest job should not appear on second page: %s", body)
	}
}

func TestControlJobsPartialFiltersActiveJobs(t *testing.T) {
	store, err := gmaps.OpenJobStore(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	activeID, err := store.CreateStartingJob(ctx, []string{"active"}, nil)
	if err != nil {
		t.Fatalf("create active: %v", err)
	}
	if err := store.StartJob(ctx, activeID); err != nil {
		t.Fatalf("start active: %v", err)
	}
	pendingID, err := store.CreateStartingJob(ctx, []string{"pending"}, nil)
	if err != nil {
		t.Fatalf("create pending: %v", err)
	}
	doneID, err := store.CreateStartingJob(ctx, []string{"done"}, nil)
	if err != nil {
		t.Fatalf("create done: %v", err)
	}
	if err := store.SetJobStatus(ctx, doneID, gmaps.JobStatusDone, nil); err != nil {
		t.Fatalf("set done: %v", err)
	}
	mux := http.NewServeMux()
	registerControlHandlers(mux, store, "gmdata/scraper-state.sqlite", nil, noopStartLauncher)
	req := httptest.NewRequest(http.MethodGet, "/ui/jobs?filter=active&page=1&page_size=10", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `data-filter="active"`) {
		t.Fatalf("active filter state missing: %s", body)
	}
	if !strings.Contains(body, `data-job-id="`+activeID+`"`) {
		t.Fatalf("active job missing: %s", body)
	}
	if strings.Contains(body, `data-job-id="`+pendingID+`"`) || strings.Contains(body, `data-job-id="`+doneID+`"`) {
		t.Fatalf("non-active jobs should not render: %s", body)
	}
}

func TestControlIndexRendersFiltersInJobHeader(t *testing.T) {
	store, err := gmaps.OpenJobStore(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	if _, err := store.CreateJob(context.Background(), []string{"coffee"}, nil, gmaps.URLsNoLang([]string{"u1"})); err != nil {
		t.Fatalf("create job: %v", err)
	}
	mux := http.NewServeMux()
	registerControlHandlers(mux, store, "gmdata/scraper-state.sqlite", nil, noopStartLauncher)
	req := httptest.NewRequest(http.MethodGet, "/?jobs_filter=active", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `class="queue-command-actions"`) || !strings.Contains(body, `data-filter-option="active"`) {
		t.Fatalf("filter controls should render in the queue command strip: %s", body)
	}
	if strings.Contains(body, "queue-toolbar") {
		t.Fatalf("filter controls should not render as a separate queue toolbar: %s", body)
	}
}

func TestJobsPaginationView(t *testing.T) {
	page := newJobsPagination(3, 10, 25, "done")
	if page.Page != 3 || page.TotalPages != 3 || page.StartItem != 21 || page.EndItem != 25 {
		t.Fatalf("page = %#v, want page 3 of 3 showing 21-25", page)
	}
	if page.Filter != "done" || page.FilterLabel != "Done" {
		t.Fatalf("filter = %q/%q, want done/Done", page.Filter, page.FilterLabel)
	}
	if !page.HasPrevious || page.HasNext {
		t.Fatalf("previous/next = %v/%v, want true/false", page.HasPrevious, page.HasNext)
	}
	page = newJobsPagination(-1, 999, 0, "invalid")
	if page.Page != 1 || page.PageSize != defaultJobsPageSize || page.Filter != defaultJobsFilter || page.TotalPages != 1 || page.StartItem != 0 || page.EndItem != 0 {
		t.Fatalf("empty normalized page = %#v", page)
	}
}

func TestControlJobEndpointReturnsCurrentJob(t *testing.T) {
	store, err := gmaps.OpenJobStore(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	jobID, err := store.CreateJob(context.Background(), []string{"coffee"}, nil, gmaps.URLsNoLang([]string{"u1"}))
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
		ID:             "job_1",
		Status:         gmaps.JobStatusRunning,
		PauseRequested: true,
		Stats:          gmaps.JobStats{Total: 3, Done: 1, Pending: 1, InProgress: 1},
	})
	if view.StatusLabel != "Pausing" {
		t.Fatalf("StatusLabel = %q, want Pausing", view.StatusLabel)
	}
	if view.ActionLabel != "Pausing" || !view.ActionDisabled {
		t.Fatalf("action = %q disabled=%v, want disabled Pausing", view.ActionLabel, view.ActionDisabled)
	}
	if view.Progress != "1 / 3 done, 1 pending, 1 active" {
		t.Fatalf("Progress = %q", view.Progress)
	}
}

func TestJobViewActionsForPausedJob(t *testing.T) {
	view := newJobView(gmaps.Job{ID: "job_1", Status: gmaps.JobStatusPaused})
	if view.StatusLabel != "Paused" {
		t.Fatalf("StatusLabel = %q, want Paused", view.StatusLabel)
	}
	if view.ActionLabel != "Resume" || view.ActionDisabled || view.ActionPath != "/api/jobs/job_1/resume" {
		t.Fatalf("action = %#v/%q disabled=%v, want enabled resume path", view.ActionLabel, view.ActionPath, view.ActionDisabled)
	}
}

func TestJobLifecycleActionMatrix(t *testing.T) {
	tests := []struct {
		name     string
		job      gmaps.Job
		label    string
		path     string
		disabled bool
	}{
		{
			name:  "running can pause",
			job:   gmaps.Job{ID: "job_1", Status: gmaps.JobStatusRunning},
			label: "Pause",
			path:  "/api/jobs/job_1/pause",
		},
		{
			name:     "running pause requested is pausing",
			job:      gmaps.Job{ID: "job_1", Status: gmaps.JobStatusRunning, PauseRequested: true},
			label:    "Pausing",
			disabled: true,
		},
		{
			name:  "blocked can resume",
			job:   gmaps.Job{ID: "job_1", Status: gmaps.JobStatusBlocked},
			label: "Resume",
			path:  "/api/jobs/job_1/resume",
		},
		{
			name:  "failed can resume",
			job:   gmaps.Job{ID: "job_1", Status: gmaps.JobStatusFailed},
			label: "Resume",
			path:  "/api/jobs/job_1/resume",
		},
		{
			name:  "pending can start when unblocked",
			job:   gmaps.Job{ID: "job_1", Status: gmaps.JobStatusPending},
			label: "Start",
			path:  "/api/jobs/job_1/start-pending",
		},
		{
			name:     "starting is disabled",
			job:      gmaps.Job{ID: "job_1", Status: gmaps.JobStatusStarting},
			label:    "Starting",
			disabled: true,
		},
		{
			name:     "done is disabled",
			job:      gmaps.Job{ID: "job_1", Status: gmaps.JobStatusDone},
			label:    "Done",
			disabled: true,
		},
	}
	t.Run("pending is queued behind active job", func(t *testing.T) {
		view := newJobViewWithQueueState(gmaps.Job{ID: "job_1", Status: gmaps.JobStatusPending}, true)
		if view.ActionLabel != "Queued" || view.ActionPath != "" || !view.ActionDisabled {
			t.Fatalf("action = label %q path %q disabled %v, want queued disabled",
				view.ActionLabel, view.ActionPath, view.ActionDisabled)
		}
	})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			view := newJobView(tt.job)
			if view.ActionLabel != tt.label || view.ActionPath != tt.path || view.ActionDisabled != tt.disabled {
				t.Fatalf("action = label %q path %q disabled %v, want %q %q %v",
					view.ActionLabel, view.ActionPath, view.ActionDisabled, tt.label, tt.path, tt.disabled)
			}
		})
	}
}

func TestControlResumeEndpointLaunchesJob(t *testing.T) {
	store, err := gmaps.OpenJobStore(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	jobID, err := store.CreateJob(context.Background(), []string{"coffee"}, nil, gmaps.URLsNoLang([]string{"u1"}))
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
	jobID, err := store.CreateJob(context.Background(), []string{"coffee"}, nil, gmaps.URLsNoLang([]string{"u1"}))
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

func TestControlUIOnlyAllowsPlaceIDs(t *testing.T) {
	if controlUIOnly("", "", "ChIJ123") {
		t.Fatal("controlUIOnly returned true for place IDs")
	}
	if !controlUIOnly("", " ", " ") {
		t.Fatal("controlUIOnly returned false for blank runnable inputs")
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

func TestStartJobPreservesSubmittedTemplateJSON(t *testing.T) {
	store := newStartStore(t)
	var captured startParams
	launcher := startLauncher(func(_ context.Context, p startParams) error {
		captured = p
		return nil
	})
	mux := http.NewServeMux()
	registerControlHandlers(mux, store, "gmdata/scraper-state.sqlite", nil, launcher)
	paramsJSON := `{"Queries":["pizza"],"OutputMode":"file","CustomNote":"keep me"}`
	rec := postStart(mux, url.Values{
		"queries":     {"pizza"},
		"output_mode": {"file"},
		"job_title":   {"pizza template"},
		"params_json": {paramsJSON},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body)
	}
	if captured.TemplateID == "" {
		t.Fatal("TemplateID was not assigned before launch")
	}
	job, err := store.GetJob(context.Background(), captured.JobID)
	if err != nil {
		t.Fatalf("load job: %v", err)
	}
	if !job.TemplateID.Valid || job.TemplateID.String != captured.TemplateID {
		t.Fatalf("job template ID = %v, want %q", job.TemplateID, captured.TemplateID)
	}
	tpl, err := store.GetJobTemplate(context.Background(), captured.TemplateID)
	if err != nil {
		t.Fatalf("load template: %v", err)
	}
	if tpl.ParamsJSON != paramsJSON {
		t.Fatalf("ParamsJSON = %q, want %q", tpl.ParamsJSON, paramsJSON)
	}
}

func TestJobLogsEndpointReturnsErrorForMissingActiveLog(t *testing.T) {
	stateDB := filepath.Join(t.TempDir(), "state.sqlite")
	store, err := gmaps.OpenJobStore(stateDB)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	jobID, err := store.CreateStartingJob(context.Background(), []string{"coffee"}, nil)
	if err != nil {
		t.Fatalf("create starting job: %v", err)
	}
	mux := http.NewServeMux()
	registerControlHandlers(mux, store, stateDB, nil, noopStartLauncher)
	req := httptest.NewRequest(http.MethodGet, "/api/jobs/"+jobID+"/logs", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "log file is not available") {
		t.Fatalf("missing log error = %q, want availability message", rec.Body.String())
	}
	if _, err := os.Stat(jobLogPath(stateDB, jobID)); !os.IsNotExist(err) {
		t.Fatalf("active missing log should not be created, stat err = %v", err)
	}
}

func TestJobLogsEndpointReturnsErrorForMissingDoneLog(t *testing.T) {
	stateDB := filepath.Join(t.TempDir(), "state.sqlite")
	store, err := gmaps.OpenJobStore(stateDB)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	jobID, err := store.CreateJob(context.Background(), []string{"coffee"}, nil, gmaps.URLsNoLang([]string{"u1"}))
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := store.SetJobStatus(context.Background(), jobID, gmaps.JobStatusDone, nil); err != nil {
		t.Fatalf("mark done: %v", err)
	}
	mux := http.NewServeMux()
	registerControlHandlers(mux, store, stateDB, nil, noopStartLauncher)
	req := httptest.NewRequest(http.MethodGet, "/api/jobs/"+jobID+"/logs", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "log file is not available") {
		t.Fatalf("missing log error = %q, want availability message", rec.Body.String())
	}
	if _, err := os.Stat(jobLogPath(stateDB, jobID)); !os.IsNotExist(err) {
		t.Fatalf("done missing log should not be created, stat err = %v", err)
	}
}

func TestJobLogsEndpointTailsExistingLog(t *testing.T) {
	stateDB := filepath.Join(t.TempDir(), "state.sqlite")
	store, err := gmaps.OpenJobStore(stateDB)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	jobID, err := store.CreateJob(context.Background(), []string{"coffee"}, nil, gmaps.URLsNoLang([]string{"u1"}))
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	path := jobLogPath(stateDB, jobID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	mux := http.NewServeMux()
	registerControlHandlers(mux, store, stateDB, nil, noopStartLauncher)
	req := httptest.NewRequest(http.MethodGet, "/api/jobs/"+jobID+"/logs?tail=2", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body)
	}
	var got jobLogsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !reflect.DeepEqual(got.Lines, []string{"two", "three"}) {
		t.Fatalf("lines = %#v, want two/three", got.Lines)
	}
}

func TestStartJobQueuesWhenRunning(t *testing.T) {
	store := newStartStore(t)
	jobID, err := store.CreateJob(context.Background(), []string{"coffee"}, nil, gmaps.URLsNoLang([]string{"u1"}))
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

func TestControlBulkUpdateStrategyDedupEndpoint(t *testing.T) {
	store := newStartStore(t)
	ctx := context.Background()
	firstID, err := store.SaveJobTemplateJSON(ctx, "", "coffee", `{"Queries":["coffee"],"OutputMode":"file","JSONOut":true}`)
	if err != nil {
		t.Fatalf("save first template: %v", err)
	}
	secondID, err := store.SaveJobTemplateJSON(ctx, "", "tea", `{"Queries":["tea"],"DedupScope":"run","OutputMode":"file","JSONOut":true}`)
	if err != nil {
		t.Fatalf("save second template: %v", err)
	}
	strategyID, err := store.SaveStrategy(ctx, "", "morning", "batch", []string{firstID, secondID})
	if err != nil {
		t.Fatalf("save strategy: %v", err)
	}
	mux := http.NewServeMux()
	registerControlHandlers(mux, store, "gmdata/scraper-state.sqlite", nil, noopStartLauncher)

	req := httptest.NewRequest(http.MethodPost, "/api/strategies/"+strategyID+"/bulk-update-dedup", strings.NewReader(`{"scope":"all"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "2 template(s) updated") {
		t.Fatalf("response = %s, want updated count", rec.Body.String())
	}
	for _, id := range []string{firstID, secondID} {
		tpl, err := store.GetJobTemplate(ctx, id)
		if err != nil {
			t.Fatalf("get template %s: %v", id, err)
		}
		var params map[string]any
		if err := json.Unmarshal([]byte(tpl.ParamsJSON), &params); err != nil {
			t.Fatalf("decode template %s: %v", id, err)
		}
		if params["DedupScope"] != "all" {
			t.Fatalf("template %s DedupScope = %#v, want all", id, params["DedupScope"])
		}
	}

	req = httptest.NewRequest(http.MethodPost, "/api/strategies/"+strategyID+"/bulk-update-dedup", strings.NewReader(`{"scope":"off"}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("off want 200, got %d: %s", rec.Code, rec.Body)
	}
	tpl, err := store.GetJobTemplate(ctx, firstID)
	if err != nil {
		t.Fatalf("get first after off: %v", err)
	}
	var params map[string]any
	if err := json.Unmarshal([]byte(tpl.ParamsJSON), &params); err != nil {
		t.Fatalf("decode first after off: %v", err)
	}
	if _, ok := params["DedupScope"]; ok {
		t.Fatalf("DedupScope should be removed by off: %#v", params)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/strategies/"+strategyID+"/bulk-update-dedup", strings.NewReader(`{"scope":"bad"}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid scope status = %d, want 400: %s", rec.Code, rec.Body)
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

func TestRunStrategyCreatesOrderedJobsWithSource(t *testing.T) {
	store := newStartStore(t)
	ctx := context.Background()
	firstID, err := store.SaveJobTemplateJSON(ctx, "", "coffee", `{"Queries":["coffee"],"OutputMode":"file","JSONOut":true,"Lang":"en"}`)
	if err != nil {
		t.Fatalf("save first template: %v", err)
	}
	secondID, err := store.SaveJobTemplateJSON(ctx, "", "tea", `{"Queries":["tea"],"OutputMode":"file","JSONOut":true,"Lang":"en"}`)
	if err != nil {
		t.Fatalf("save second template: %v", err)
	}
	strategyID, err := store.SaveStrategy(ctx, "", "morning", "batch", []string{firstID, secondID})
	if err != nil {
		t.Fatalf("save strategy: %v", err)
	}
	var launched []startParams
	launcher := startLauncher(func(_ context.Context, p startParams) error {
		jobs, err := store.ListJobs(ctx)
		if err != nil {
			t.Fatalf("list jobs during launch: %v", err)
		}
		if len(jobs) != 2 {
			t.Fatalf("jobs visible during launch = %d, want 2", len(jobs))
		}
		launched = append(launched, p)
		return nil
	})
	mux := http.NewServeMux()
	registerControlHandlers(mux, store, "gmdata/scraper-state.sqlite", nil, launcher)
	req := httptest.NewRequest(http.MethodPost, "/api/strategies/"+strategyID+"/run", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body)
	}
	if len(launched) != 1 || launched[0].TemplateID != firstID || launched[0].StrategyID != strategyID {
		t.Fatalf("launched = %#v, want first template sourced launch", launched)
	}
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("jobs len = %d, want 2", len(jobs))
	}
	var sourced int
	for _, job := range jobs {
		if job.StrategyID.String == strategyID && job.StrategyRunID.String != "" {
			sourced++
		}
	}
	if sourced != 2 {
		t.Fatalf("sourced jobs = %d, want 2", sourced)
	}
}

func TestRunStrategyCreatesAllTemplateJobs(t *testing.T) {
	store := newStartStore(t)
	ctx := context.Background()
	var templateIDs []string
	for i := 0; i < 71; i++ {
		params := `{"Queries":["restaurant ` + strconv.Itoa(i+1) + `"],"OutputMode":"file","JSONOut":true,"Lang":"en"}`
		id, err := store.SaveJobTemplateJSON(ctx, "", "template "+strconv.Itoa(i+1), params)
		if err != nil {
			t.Fatalf("save template %d: %v", i+1, err)
		}
		templateIDs = append(templateIDs, id)
	}
	strategyID, err := store.SaveStrategy(ctx, "", "production", "batch", templateIDs)
	if err != nil {
		t.Fatalf("save strategy: %v", err)
	}
	mux := http.NewServeMux()
	registerControlHandlers(mux, store, "gmdata/scraper-state.sqlite", nil, noopStartLauncher)
	req := httptest.NewRequest(http.MethodPost, "/api/strategies/"+strategyID+"/run", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body)
	}
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 71 {
		t.Fatalf("jobs len = %d, want 71", len(jobs))
	}
}

func TestRunStrategyPreflightFailureCreatesNoJobs(t *testing.T) {
	t.Setenv("DSN", "")
	store := newStartStore(t)
	ctx := context.Background()
	firstID, err := store.SaveJobTemplateJSON(ctx, "", "coffee", `{"Queries":["coffee"],"OutputMode":"file","JSONOut":true,"Lang":"en"}`)
	if err != nil {
		t.Fatalf("save first template: %v", err)
	}
	secondID, err := store.SaveJobTemplateJSON(ctx, "", "database template", `{"Queries":["tea"],"OutputMode":"database","Lang":"en"}`)
	if err != nil {
		t.Fatalf("save second template: %v", err)
	}
	thirdID, err := store.SaveJobTemplateJSON(ctx, "", "pastry", `{"Queries":["pastry"],"OutputMode":"file","JSONOut":true,"Lang":"en"}`)
	if err != nil {
		t.Fatalf("save third template: %v", err)
	}
	strategyID, err := store.SaveStrategy(ctx, "", "morning", "batch", []string{firstID, secondID, thirdID})
	if err != nil {
		t.Fatalf("save strategy: %v", err)
	}
	mux := http.NewServeMux()
	registerControlHandlers(mux, store, "gmdata/scraper-state.sqlite", nil, noopStartLauncher)
	req := httptest.NewRequest(http.MethodPost, "/api/strategies/"+strategyID+"/run", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `template 2 "database template"`) || !strings.Contains(body, "database output mode requires DSN") {
		t.Fatalf("error body = %q, want template context and DSN error", body)
	}
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("jobs len = %d, want 0", len(jobs))
	}
}

func TestJobViewUsesTemplateNameAsDisplayTitle(t *testing.T) {
	view := newJobViewWithQueueState(gmaps.Job{
		ID:           "job_1",
		Queries:      []string{"restaurants", "cafes"},
		Status:       gmaps.JobStatusPending,
		TemplateName: sql.NullString{String: "Downtown restaurants", Valid: true},
	}, false)
	if view.DisplayTitle != "Downtown restaurants" {
		t.Fatalf("DisplayTitle = %q, want template name", view.DisplayTitle)
	}
	if view.QueriesPreview != "restaurants +1" {
		t.Fatalf("QueriesPreview = %q, want query preview fallback value", view.QueriesPreview)
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
	if err := runJobQueueOnce(ctx, store, "gmdata/scraper-state.sqlite", nil, launcher); err != nil {
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
	if err := runJobQueueOnce(ctx, store, "gmdata/scraper-state.sqlite", nil, launcher); err != nil {
		t.Fatalf("run queue: %v", err)
	}
	if called {
		t.Fatal("queue launched after failed job")
	}
}

func TestRunJobQueueOnceAutoResumesTimeoutPausedJob(t *testing.T) {
	oldDelay := timeoutAutoResumeDelay
	timeoutAutoResumeDelay = func() time.Duration { return 0 }
	t.Cleanup(func() { timeoutAutoResumeDelay = oldDelay })

	store := newStartStore(t)
	ctx := context.Background()
	jobID, err := store.CreateJob(ctx, []string{"coffee"}, gmaps.Config{OutputMode: "file"}, gmaps.URLsNoLang([]string{"u1"}))
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := store.StartJob(ctx, jobID); err != nil {
		t.Fatalf("start job: %v", err)
	}
	if _, err := store.ClaimNextURL(ctx, jobID); err != nil {
		t.Fatalf("claim url: %v", err)
	}
	if err := store.RecoverStaleActiveJob(ctx, jobID, errors.New(gmaps.JobTimeoutError)); err != nil {
		t.Fatalf("recover timeout job: %v", err)
	}

	var resumed string
	resume := resumeLauncher(func(_ context.Context, id string) error {
		resumed = id
		return nil
	})
	startCalled := false
	start := startLauncher(func(_ context.Context, _ startParams) error {
		startCalled = true
		return nil
	})
	if err := runJobQueueOnce(ctx, store, "gmdata/scraper-state.sqlite", resume, start); err != nil {
		t.Fatalf("run queue: %v", err)
	}
	if resumed != jobID {
		t.Fatalf("resumed = %q, want %q", resumed, jobID)
	}
	if startCalled {
		t.Fatal("started pending job instead of resuming timeout job")
	}
}

func TestRunJobQueueOnceAutoResumesTimedOutDiscovery(t *testing.T) {
	oldDelay := timeoutAutoResumeDelay
	timeoutAutoResumeDelay = func() time.Duration { return 0 }
	t.Cleanup(func() { timeoutAutoResumeDelay = oldDelay })

	store := newStartStore(t)
	ctx := context.Background()
	jobID, err := store.CreateStartingJob(ctx, []string{"coffee"}, gmaps.Config{OutputMode: "file"})
	if err != nil {
		t.Fatalf("create starting job: %v", err)
	}
	if err := store.RecoverStaleActiveJob(ctx, jobID, errors.New(gmaps.JobTimeoutError)); err != nil {
		t.Fatalf("recover timeout: %v", err)
	}

	var resumed string
	resume := resumeLauncher(func(_ context.Context, id string) error {
		resumed = id
		return nil
	})
	if err := runJobQueueOnce(ctx, store, "gmdata/scraper-state.sqlite", resume, noopStartLauncher); err != nil {
		t.Fatalf("run queue: %v", err)
	}
	if resumed != jobID {
		t.Fatalf("resumed = %q, want %q", resumed, jobID)
	}
}

func TestRunJobQueueOnceDoesNotAutoResumeManualPause(t *testing.T) {
	oldDelay := timeoutAutoResumeDelay
	timeoutAutoResumeDelay = func() time.Duration { return 0 }
	t.Cleanup(func() { timeoutAutoResumeDelay = oldDelay })

	store := newStartStore(t)
	ctx := context.Background()
	jobID, err := store.CreateJob(ctx, []string{"coffee"}, gmaps.Config{OutputMode: "file"}, gmaps.URLsNoLang([]string{"u1"}))
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := store.SetJobStatus(ctx, jobID, gmaps.JobStatusPaused, errors.New("operator paused")); err != nil {
		t.Fatalf("pause job: %v", err)
	}
	called := false
	resume := resumeLauncher(func(_ context.Context, _ string) error {
		called = true
		return nil
	})
	if err := runJobQueueOnce(ctx, store, "gmdata/scraper-state.sqlite", resume, noopStartLauncher); err != nil {
		t.Fatalf("run queue: %v", err)
	}
	if called {
		t.Fatal("manual pause was auto-resumed")
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
	want := []string{"-job", "job_db", "-state-db", "/data/state.sqlite"}
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

func TestBatchDeleteJobsEndpoint(t *testing.T) {
	ctx := context.Background()
	store := newStartStore(t)
	stateDB := filepath.Join(t.TempDir(), "scraper-state.sqlite")

	doneID, err := store.CreateStartingJob(ctx, []string{"done"}, nil)
	if err != nil {
		t.Fatalf("create done: %v", err)
	}
	if err := store.SetJobStatus(ctx, doneID, gmaps.JobStatusDone, nil); err != nil {
		t.Fatalf("set done: %v", err)
	}
	activeID, err := store.CreateStartingJob(ctx, []string{"active"}, nil)
	if err != nil {
		t.Fatalf("create active: %v", err)
	}
	if err := store.StartJob(ctx, activeID); err != nil {
		t.Fatalf("start active: %v", err)
	}

	// Seed a log file for the deletable job to confirm cleanup.
	logPath := jobLogPath(stateDB, doneID)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}
	if err := os.WriteFile(logPath, []byte("log"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	mux := http.NewServeMux()
	registerControlHandlers(mux, store, stateDB, nil, noopStartLauncher)

	payload, _ := json.Marshal(map[string]any{"job_ids": []string{doneID, activeID, "missing"}})
	req := httptest.NewRequest(http.MethodPost, "/api/jobs/batch-delete", bytes.NewReader(payload))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Results []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := map[string]string{}
	for _, r := range resp.Results {
		got[r.ID] = r.Status
	}
	if got[doneID] != "deleted" || got[activeID] != "skipped_active" || got["missing"] != "not_found" {
		t.Fatalf("results = %+v", got)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("log file for deleted job should be removed, stat err = %v", err)
	}
}

func TestBatchDeleteJobTemplatesEndpoint(t *testing.T) {
	ctx := context.Background()
	store := newStartStore(t)

	referenced, err := store.SaveJobTemplate(ctx, "ref", map[string]any{"q": "ref"})
	if err != nil {
		t.Fatalf("save ref: %v", err)
	}
	free, err := store.SaveJobTemplate(ctx, "free", map[string]any{"q": "free"})
	if err != nil {
		t.Fatalf("save free: %v", err)
	}
	if _, err := store.SaveStrategy(ctx, "", "S", "", []string{referenced}); err != nil {
		t.Fatalf("save strategy: %v", err)
	}

	mux := http.NewServeMux()
	registerControlHandlers(mux, store, "gmdata/scraper-state.sqlite", nil, noopStartLauncher)

	payload, _ := json.Marshal(map[string]any{"template_ids": []string{free, referenced}})
	req := httptest.NewRequest(http.MethodPost, "/api/job-templates/batch-delete", bytes.NewReader(payload))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Results []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := map[string]string{}
	for _, r := range resp.Results {
		got[r.ID] = r.Status
	}
	if got[free] != "deleted" || got[referenced] != "skipped_referenced" {
		t.Fatalf("results = %+v", got)
	}
}

func TestSweepLeakedProcessesCleansRows(t *testing.T) {
	ctx := context.Background()
	store := newStartStore(t)

	// A pid that does not match our exe/chrome (init/pid 1) must not be killed,
	// only have its stale row cleaned.
	if err := store.RecordJobProcess(ctx, "leaked", 1, 1); err != nil {
		t.Fatalf("record: %v", err)
	}
	sweepLeakedProcesses(ctx, store)
	procs, err := store.ListJobProcesses(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(procs) != 0 {
		t.Fatalf("rows after sweep = %d, want 0", len(procs))
	}
}

func TestSpawnProcessTimeoutRecoversJob(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh is required for subprocess timeout test")
	}
	t.Setenv("SCRAPER_JOB_TIMEOUT", "100ms")

	ctx := context.Background()
	store := newStartStore(t)
	jobID, err := store.CreateJob(ctx, []string{"coffee"}, nil, gmaps.URLsNoLang([]string{"u1"}))
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := store.StartJob(ctx, jobID); err != nil {
		t.Fatalf("start job: %v", err)
	}
	if _, err := store.ClaimNextURL(ctx, jobID); err != nil {
		t.Fatalf("claim url: %v", err)
	}

	logPath := filepath.Join(t.TempDir(), "job.log")
	if err := spawnProcess(store, jobID, "/bin/sh", []string{"-c", "sleep 2"}, logPath, nil); err != nil {
		t.Fatalf("spawn process: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		job, jobErr := store.GetJob(ctx, jobID)
		stats, statsErr := store.JobStats(ctx, jobID)
		procs, procsErr := store.ListJobProcesses(ctx)
		if jobErr == nil && statsErr == nil && procsErr == nil {
			last = fmt.Sprintf("status=%s pending=%d in_progress=%d procs=%d", job.Status, stats.Pending, stats.InProgress, len(procs))
			if job.Status == gmaps.JobStatusPaused && stats.Pending == 1 && stats.InProgress == 0 && len(procs) == 0 {
				return
			}
		} else {
			last = fmt.Sprintf("jobErr=%v statsErr=%v procsErr=%v", jobErr, statsErr, procsErr)
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed-out process did not recover job; last state: %s", last)
}

func TestAllowStallAutoResumeCap(t *testing.T) {
	jobID := "cap-" + t.Name()
	for i := 0; i < maxStallAutoResumes; i++ {
		if !allowStallAutoResume(jobID) {
			t.Fatalf("attempt %d denied, want allowed (cap %d)", i+1, maxStallAutoResumes)
		}
	}
	if allowStallAutoResume(jobID) {
		t.Fatalf("attempt %d allowed, want denied past cap", maxStallAutoResumes+1)
	}
	// Other jobs have their own budget.
	if !allowStallAutoResume(jobID + "-other") {
		t.Fatal("fresh job denied, want allowed")
	}
}

func TestSpawnProcessStallExitAutoResumes(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh is required for subprocess stall test")
	}
	oldDelay := stallResumeDelay
	stallResumeDelay = 10 * time.Millisecond
	t.Cleanup(func() { stallResumeDelay = oldDelay })

	ctx := context.Background()
	store := newStartStore(t)
	jobID, err := store.CreateJob(ctx, []string{"coffee"}, nil, gmaps.URLsNoLang([]string{"u1"}))
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := store.StartJob(ctx, jobID); err != nil {
		t.Fatalf("start job: %v", err)
	}
	if _, err := store.ClaimNextURL(ctx, jobID); err != nil {
		t.Fatalf("claim url: %v", err)
	}

	resumed := make(chan string, 1)
	logPath := filepath.Join(t.TempDir(), "job.log")
	cmd := []string{"-c", fmt.Sprintf("exit %d", exitCodeStallWatchdog)}
	if err := spawnProcess(store, jobID, "/bin/sh", cmd, logPath, func(id string) { resumed <- id }); err != nil {
		t.Fatalf("spawn process: %v", err)
	}

	select {
	case id := <-resumed:
		if id != jobID {
			t.Fatalf("onStallExit job = %q, want %q", id, jobID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("onStallExit was not called after stall-watchdog exit")
	}

	// Recovery and cleanup run before the resume hook, so the job must
	// already be paused with its in-progress URL reset and its registry
	// row dropped.
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Status != gmaps.JobStatusPaused {
		t.Fatalf("status = %s, want %s", job.Status, gmaps.JobStatusPaused)
	}
	stats, err := store.JobStats(ctx, jobID)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Pending != 1 || stats.InProgress != 0 {
		t.Fatalf("stats = pending %d in_progress %d, want 1/0", stats.Pending, stats.InProgress)
	}
	procs, err := store.ListJobProcesses(ctx)
	if err != nil {
		t.Fatalf("list procs: %v", err)
	}
	if len(procs) != 0 {
		t.Fatalf("job process rows = %d, want 0", len(procs))
	}
}

func TestSpawnProcessStallExitRespectsCap(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh is required for subprocess stall test")
	}
	oldDelay := stallResumeDelay
	stallResumeDelay = 10 * time.Millisecond
	t.Cleanup(func() { stallResumeDelay = oldDelay })

	ctx := context.Background()
	store := newStartStore(t)
	jobID, err := store.CreateJob(ctx, []string{"coffee"}, nil, gmaps.URLsNoLang([]string{"u1"}))
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := store.StartJob(ctx, jobID); err != nil {
		t.Fatalf("start job: %v", err)
	}

	// Exhaust the auto-resume budget up front.
	stallResumesMu.Lock()
	stallResumes[jobID] = maxStallAutoResumes
	stallResumesMu.Unlock()

	resumed := make(chan string, 1)
	logPath := filepath.Join(t.TempDir(), "job.log")
	cmd := []string{"-c", fmt.Sprintf("exit %d", exitCodeStallWatchdog)}
	if err := spawnProcess(store, jobID, "/bin/sh", cmd, logPath, func(id string) { resumed <- id }); err != nil {
		t.Fatalf("spawn process: %v", err)
	}

	// Wait for the wait-goroutine to finish cleanup, then confirm the hook
	// never fired and the job was still recovered to paused.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		procs, err := store.ListJobProcesses(ctx)
		if err == nil && len(procs) == 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	select {
	case id := <-resumed:
		t.Fatalf("onStallExit fired for job %q despite cap", id)
	case <-time.After(200 * time.Millisecond):
	}
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Status != gmaps.JobStatusPaused {
		t.Fatalf("status = %s, want %s", job.Status, gmaps.JobStatusPaused)
	}
}

func TestJobsPanelRendersBatchSelectUI(t *testing.T) {
	ctx := context.Background()
	store := newStartStore(t)
	doneID, err := store.CreateStartingJob(ctx, []string{"done"}, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.SetJobStatus(ctx, doneID, gmaps.JobStatusDone, nil); err != nil {
		t.Fatalf("set done: %v", err)
	}

	mux := http.NewServeMux()
	registerControlHandlers(mux, store, "gmdata/scraper-state.sqlite", nil, noopStartLauncher)
	req := httptest.NewRequest(http.MethodGet, "/ui/jobs", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"jobs-bulk-bar", `class="job-select"`, "batchDeleteJobs"} {
		if !strings.Contains(body, want) {
			t.Fatalf("jobs panel missing %q", want)
		}
	}
}
