package gmaps

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
)

func newTestJobStore(t *testing.T) *JobStore {
	t.Helper()
	store, err := OpenJobStore(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatalf("open job store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("close job store: %v", err)
		}
	})
	return store
}

func TestJobStoreClaimOrderingAndDoneSkip(t *testing.T) {
	ctx := context.Background()
	store := newTestJobStore(t)
	jobID, err := store.CreateJob(ctx, []string{"coffee"}, map[string]string{"lang": "en"}, []string{"u1", "u2", "u3"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := store.StartJob(ctx, jobID); err != nil {
		t.Fatalf("start job: %v", err)
	}

	first, err := store.ClaimNextURL(ctx, jobID)
	if err != nil {
		t.Fatalf("claim first: %v", err)
	}
	if first.URL != "u1" || first.Position != 0 {
		t.Fatalf("first claim = %q/%d, want u1/0", first.URL, first.Position)
	}
	if err := store.MarkURLDone(ctx, first.ID); err != nil {
		t.Fatalf("mark done: %v", err)
	}

	second, err := store.ClaimNextURL(ctx, jobID)
	if err != nil {
		t.Fatalf("claim second: %v", err)
	}
	if second.URL != "u2" || second.Position != 1 {
		t.Fatalf("second claim = %q/%d, want u2/1", second.URL, second.Position)
	}
}

func TestJobStoreResetInProgress(t *testing.T) {
	ctx := context.Background()
	store := newTestJobStore(t)
	jobID, err := store.CreateJob(ctx, []string{"coffee"}, nil, []string{"u1"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if _, err := store.ClaimNextURL(ctx, jobID); err != nil {
		t.Fatalf("claim: %v", err)
	}
	stats, err := store.JobStats(ctx, jobID)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.InProgress != 1 {
		t.Fatalf("in-progress before reset = %d, want 1", stats.InProgress)
	}

	if err := store.ResetInProgress(ctx, jobID); err != nil {
		t.Fatalf("reset: %v", err)
	}
	stats, err = store.JobStats(ctx, jobID)
	if err != nil {
		t.Fatalf("stats after reset: %v", err)
	}
	if stats.Pending != 1 || stats.InProgress != 0 {
		t.Fatalf("after reset pending=%d in_progress=%d, want 1/0", stats.Pending, stats.InProgress)
	}
}

func TestJobStorePauseFlagBlocksClaim(t *testing.T) {
	ctx := context.Background()
	store := newTestJobStore(t)
	jobID, err := store.CreateJob(ctx, []string{"coffee"}, nil, []string{"u1"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := store.RequestPause(ctx, jobID); err != nil {
		t.Fatalf("request pause: %v", err)
	}

	if _, err := store.ClaimNextURL(ctx, jobID); !errors.Is(err, ErrJobPaused) {
		t.Fatalf("claim error = %v, want ErrJobPaused", err)
	}
	stats, err := store.JobStats(ctx, jobID)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.InProgress != 0 || stats.Pending != 1 {
		t.Fatalf("paused claim changed URL state: pending=%d in_progress=%d, want 1/0", stats.Pending, stats.InProgress)
	}
	paused, err := store.PauseRequested(ctx, jobID)
	if err != nil {
		t.Fatalf("pause requested: %v", err)
	}
	if !paused {
		t.Fatal("pause flag was not persisted")
	}
}

func TestJobStoreClaimResumeClaimsOnceAndClearsPause(t *testing.T) {
	ctx := context.Background()
	store := newTestJobStore(t)
	jobID, err := store.CreateJob(ctx, []string{"coffee"}, map[string]string{"lang": "en"}, []string{"u1"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := store.RequestPause(ctx, jobID); err != nil {
		t.Fatalf("request pause: %v", err)
	}

	job, err := store.ClaimResume(ctx, jobID)
	if err != nil {
		t.Fatalf("claim resume: %v", err)
	}
	if job.ID != jobID {
		t.Fatalf("claimed job ID = %q, want %q", job.ID, jobID)
	}
	if job.Status != JobStatusRunning {
		t.Fatalf("claimed job status = %q, want %q", job.Status, JobStatusRunning)
	}
	if job.PauseRequested {
		t.Fatal("claim resume did not clear pause flag")
	}

	if _, err := store.ClaimResume(ctx, jobID); !errors.Is(err, ErrJobNotResumable) {
		t.Fatalf("second claim error = %v, want ErrJobNotResumable", err)
	}
}

func TestJobStoreClaimResumeRejectsDoneJob(t *testing.T) {
	ctx := context.Background()
	store := newTestJobStore(t)
	jobID, err := store.CreateJob(ctx, []string{"coffee"}, nil, []string{"u1"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := store.SetJobStatus(ctx, jobID, JobStatusDone, nil); err != nil {
		t.Fatalf("set done: %v", err)
	}

	if _, err := store.ClaimResume(ctx, jobID); !errors.Is(err, ErrJobNotResumable) {
		t.Fatalf("claim done job error = %v, want ErrJobNotResumable", err)
	}
}

func TestJobStoreCreateStartingJobQueuesBehindActiveJob(t *testing.T) {
	ctx := context.Background()
	store := newTestJobStore(t)
	jobID, err := store.CreateStartingJob(ctx, []string{"coffee"}, map[string]string{"output": "file"})
	if err != nil {
		t.Fatalf("create starting job: %v", err)
	}
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("get starting job: %v", err)
	}
	if job.Status != JobStatusStarting {
		t.Fatalf("status = %q, want starting", job.Status)
	}
	queuedID, err := store.CreateStartingJob(ctx, []string{"tea"}, nil)
	if err != nil {
		t.Fatalf("create queued job: %v", err)
	}
	queued, err := store.GetJob(ctx, queuedID)
	if err != nil {
		t.Fatalf("get queued job: %v", err)
	}
	if queued.Status != JobStatusPending {
		t.Fatalf("queued status = %q, want pending", queued.Status)
	}
}

func TestJobStoreClaimNextPendingJobAfterDone(t *testing.T) {
	ctx := context.Background()
	store := newTestJobStore(t)
	firstID, err := store.CreateStartingJob(ctx, []string{"coffee"}, nil)
	if err != nil {
		t.Fatalf("create first job: %v", err)
	}
	secondID, err := store.CreateStartingJob(ctx, []string{"tea"}, nil)
	if err != nil {
		t.Fatalf("create second job: %v", err)
	}
	if err := store.SetJobStatus(ctx, firstID, JobStatusDone, nil); err != nil {
		t.Fatalf("set done: %v", err)
	}
	peek, err := store.NextPendingJobAfterDone(ctx)
	if err != nil {
		t.Fatalf("next pending: %v", err)
	}
	if peek.ID != secondID {
		t.Fatalf("peek ID = %q, want %q", peek.ID, secondID)
	}
	claimed, err := store.ClaimNextPendingJob(ctx)
	if err != nil {
		t.Fatalf("claim next pending: %v", err)
	}
	if claimed.ID != secondID || claimed.Status != JobStatusStarting {
		t.Fatalf("claimed = %q/%q, want %q/starting", claimed.ID, claimed.Status, secondID)
	}
	if _, err := store.ClaimNextPendingJob(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second claim error = %v, want sql.ErrNoRows", err)
	}
}

func TestJobStoreClaimPendingJobByID(t *testing.T) {
	ctx := context.Background()
	store := newTestJobStore(t)
	activeID, err := store.CreateStartingJob(ctx, []string{"active"}, nil)
	if err != nil {
		t.Fatalf("create active job: %v", err)
	}
	if err := store.StartJob(ctx, activeID); err != nil {
		t.Fatalf("start active job: %v", err)
	}
	pendingID, err := store.CreateStartingJob(ctx, []string{"pending"}, nil)
	if err != nil {
		t.Fatalf("create pending job: %v", err)
	}
	if _, err := store.ClaimPendingJob(ctx, pendingID); !errors.Is(err, ErrActiveJobExists) {
		t.Fatalf("claim with active error = %v, want ErrActiveJobExists", err)
	}
	if err := store.SetJobStatus(ctx, activeID, JobStatusDone, nil); err != nil {
		t.Fatalf("mark active done: %v", err)
	}
	claimed, err := store.ClaimPendingJob(ctx, pendingID)
	if err != nil {
		t.Fatalf("claim pending: %v", err)
	}
	if claimed.ID != pendingID || claimed.Status != JobStatusStarting {
		t.Fatalf("claimed job = %s/%s, want %s/starting", claimed.ID, claimed.Status, pendingID)
	}
	if _, err := store.ClaimPendingJob(ctx, activeID); !errors.Is(err, ErrJobNotPending) {
		t.Fatalf("claim done error = %v, want ErrJobNotPending", err)
	}
}

func TestJobStoreRecoverStaleActiveJob(t *testing.T) {
	ctx := context.Background()
	store := newTestJobStore(t)
	runningID, err := store.CreateJob(ctx, []string{"running"}, nil, []string{"u1"})
	if err != nil {
		t.Fatalf("create running job: %v", err)
	}
	if err := store.StartJob(ctx, runningID); err != nil {
		t.Fatalf("start running job: %v", err)
	}
	if _, err := store.ClaimNextURL(ctx, runningID); err != nil {
		t.Fatalf("claim url: %v", err)
	}
	if err := store.RecoverStaleActiveJob(ctx, runningID, errors.New("process stopped before completion")); err != nil {
		t.Fatalf("recover running: %v", err)
	}
	job, err := store.GetJob(ctx, runningID)
	if err != nil {
		t.Fatalf("get recovered running job: %v", err)
	}
	if job.Status != JobStatusPaused || !job.LastError.Valid {
		t.Fatalf("running recovery status/error = %s/%v, want paused with error", job.Status, job.LastError)
	}
	stats, err := store.JobStats(ctx, runningID)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.InProgress != 0 || stats.Pending != 1 {
		t.Fatalf("stats after recovery = %+v, want in_progress reset to pending", stats)
	}

	startingID, err := store.CreateStartingJob(ctx, []string{"starting"}, nil)
	if err != nil {
		t.Fatalf("create starting job: %v", err)
	}
	if err := store.RecoverStaleActiveJob(ctx, startingID, errors.New("process stopped before completion")); err != nil {
		t.Fatalf("recover starting: %v", err)
	}
	job, err = store.GetJob(ctx, startingID)
	if err != nil {
		t.Fatalf("get recovered starting job: %v", err)
	}
	if job.Status != JobStatusFailed {
		t.Fatalf("starting recovery status = %s, want failed", job.Status)
	}
	if err := store.RecoverStaleActiveJob(ctx, startingID, nil); !errors.Is(err, ErrJobNotStale) {
		t.Fatalf("recover failed job error = %v, want ErrJobNotStale", err)
	}
}

func TestJobStoreListJobsPage(t *testing.T) {
	ctx := context.Background()
	store := newTestJobStore(t)
	var ids []string
	for _, query := range []string{"one", "two", "three"} {
		id, err := store.CreateJob(ctx, []string{query}, nil, []string{"url-" + query})
		if err != nil {
			t.Fatalf("create %s: %v", query, err)
		}
		ids = append(ids, id)
	}
	total, err := store.CountJobs(ctx)
	if err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if total != 3 {
		t.Fatalf("total = %d, want 3", total)
	}
	jobs, err := store.ListJobsPage(ctx, 2, 1)
	if err != nil {
		t.Fatalf("list page: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("jobs len = %d, want 2", len(jobs))
	}
	if jobs[0].ID != ids[1] || jobs[1].ID != ids[0] {
		t.Fatalf("page IDs = %q/%q, want %q/%q", jobs[0].ID, jobs[1].ID, ids[1], ids[0])
	}
}

func TestJobStoreListJobsPageFiltered(t *testing.T) {
	ctx := context.Background()
	store := newTestJobStore(t)
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
	if err := store.SetJobStatus(ctx, doneID, JobStatusDone, nil); err != nil {
		t.Fatalf("set done: %v", err)
	}
	blockedID, err := store.CreateStartingJob(ctx, []string{"blocked"}, nil)
	if err != nil {
		t.Fatalf("create blocked: %v", err)
	}
	if err := store.SetJobStatus(ctx, blockedID, JobStatusBlocked, nil); err != nil {
		t.Fatalf("set blocked: %v", err)
	}

	tests := []struct {
		filter string
		want   []string
	}{
		{filter: "pending", want: []string{pendingID}},
		{filter: "active", want: []string{activeID}},
		{filter: "done", want: []string{doneID}},
		{filter: "unknown", want: []string{blockedID, doneID, pendingID, activeID}},
	}
	for _, tt := range tests {
		t.Run(tt.filter, func(t *testing.T) {
			total, err := store.CountJobsFiltered(ctx, tt.filter)
			if err != nil {
				t.Fatalf("count: %v", err)
			}
			if total != len(tt.want) {
				t.Fatalf("total = %d, want %d", total, len(tt.want))
			}
			jobs, err := store.ListJobsPageFiltered(ctx, tt.filter, 10, 0)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if len(jobs) != len(tt.want) {
				t.Fatalf("jobs len = %d, want %d", len(jobs), len(tt.want))
			}
			for i, want := range tt.want {
				if jobs[i].ID != want {
					t.Fatalf("jobs[%d] = %q, want %q", i, jobs[i].ID, want)
				}
			}
		})
	}
}

func TestJobStoreSaveAndListJobTemplates(t *testing.T) {
	ctx := context.Background()
	store := newTestJobStore(t)
	params := map[string]any{"queries": []string{"coffee"}, "lang": "en"}
	id, err := store.SaveJobTemplate(ctx, "coffee [file/en]", params)
	if err != nil {
		t.Fatalf("save template: %v", err)
	}
	if _, err := store.SaveJobTemplate(ctx, "coffee [file/en]", params); err != nil {
		t.Fatalf("upsert template: %v", err)
	}
	templates, err := store.ListJobTemplates(ctx)
	if err != nil {
		t.Fatalf("list templates: %v", err)
	}
	if len(templates) != 1 {
		t.Fatalf("templates len = %d, want 1", len(templates))
	}
	if templates[0].ID != id || templates[0].Name != "coffee [file/en]" {
		t.Fatalf("template = %#v, want id %q and saved name", templates[0], id)
	}
}

func TestJobStoreExportReusableConfigIncludesOrderedStrategies(t *testing.T) {
	ctx := context.Background()
	store := newTestJobStore(t)
	firstID, err := store.SaveJobTemplateJSON(ctx, "", "coffee", `{"Queries":["coffee"]}`)
	if err != nil {
		t.Fatalf("save first template: %v", err)
	}
	secondID, err := store.SaveJobTemplateJSON(ctx, "", "tea", `{"Queries":["tea"]}`)
	if err != nil {
		t.Fatalf("save second template: %v", err)
	}
	strategyID, err := store.SaveStrategy(ctx, "", "morning", "ordered", []string{secondID, firstID})
	if err != nil {
		t.Fatalf("save strategy: %v", err)
	}

	cfg, err := store.ExportReusableConfig(ctx)
	if err != nil {
		t.Fatalf("export config: %v", err)
	}
	if cfg.Version != ConfigExportVersion || len(cfg.Templates) != 2 || len(cfg.Strategies) != 1 {
		t.Fatalf("export = %#v, want version with 2 templates and 1 strategy", cfg)
	}
	if cfg.Strategies[0].ID != strategyID {
		t.Fatalf("strategy id = %q, want %q", cfg.Strategies[0].ID, strategyID)
	}
	if got := cfg.Strategies[0].TemplateIDs; len(got) != 2 || got[0] != secondID || got[1] != firstID {
		t.Fatalf("strategy template order = %#v, want [%q %q]", got, secondID, firstID)
	}
}

func TestJobStoreExportReusableConfigSelection(t *testing.T) {
	ctx := context.Background()
	store := newTestJobStore(t)
	firstID, err := store.SaveJobTemplateJSON(ctx, "", "coffee", `{"Queries":["coffee"]}`)
	if err != nil {
		t.Fatalf("save first template: %v", err)
	}
	secondID, err := store.SaveJobTemplateJSON(ctx, "", "tea", `{"Queries":["tea"]}`)
	if err != nil {
		t.Fatalf("save second template: %v", err)
	}
	thirdID, err := store.SaveJobTemplateJSON(ctx, "", "pastry", `{"Queries":["pastry"]}`)
	if err != nil {
		t.Fatalf("save third template: %v", err)
	}
	strategyID, err := store.SaveStrategy(ctx, "", "morning", "ordered", []string{secondID, firstID})
	if err != nil {
		t.Fatalf("save strategy: %v", err)
	}

	cfg, err := store.ExportReusableConfigSelection(ctx, nil, []string{strategyID})
	if err != nil {
		t.Fatalf("export strategy selection: %v", err)
	}
	if len(cfg.Strategies) != 1 {
		t.Fatalf("strategies len = %d, want 1", len(cfg.Strategies))
	}
	if got := cfg.Strategies[0].TemplateIDs; len(got) != 2 || got[0] != secondID || got[1] != firstID {
		t.Fatalf("strategy template ids = %#v, want selected strategy order", got)
	}
	exportedTemplates := map[string]bool{}
	for _, tpl := range cfg.Templates {
		exportedTemplates[tpl.ID] = true
	}
	if !exportedTemplates[firstID] || !exportedTemplates[secondID] || exportedTemplates[thirdID] {
		t.Fatalf("exported templates = %#v, want only strategy templates", exportedTemplates)
	}

	cfg, err = store.ExportReusableConfigSelection(ctx, []string{thirdID}, nil)
	if err != nil {
		t.Fatalf("export template selection: %v", err)
	}
	if len(cfg.Templates) != 1 || cfg.Templates[0].ID != thirdID || len(cfg.Strategies) != 0 {
		t.Fatalf("template-only export = %#v, want selected template and no strategies", cfg)
	}

	if _, err := store.ExportReusableConfigSelection(ctx, []string{"tpl_missing"}, nil); !errors.Is(err, ErrConfigExportInvalid) {
		t.Fatalf("missing template err = %v, want export invalid", err)
	}
}

func TestJobStoreImportReusableConfigPreservesStrategyOrder(t *testing.T) {
	ctx := context.Background()
	source := newTestJobStore(t)
	firstID, err := source.SaveJobTemplateJSON(ctx, "", "coffee", `{"Queries":["coffee"]}`)
	if err != nil {
		t.Fatalf("save first template: %v", err)
	}
	secondID, err := source.SaveJobTemplateJSON(ctx, "", "tea", `{"Queries":["tea"]}`)
	if err != nil {
		t.Fatalf("save second template: %v", err)
	}
	if _, err := source.SaveStrategy(ctx, "", "morning", "ordered", []string{secondID, firstID}); err != nil {
		t.Fatalf("save strategy: %v", err)
	}
	cfg, err := source.ExportReusableConfig(ctx)
	if err != nil {
		t.Fatalf("export config: %v", err)
	}

	target := newTestJobStore(t)
	summary, err := target.ImportReusableConfig(ctx, cfg, ConfigImportRename)
	if err != nil {
		t.Fatalf("import config: %v", err)
	}
	if summary.Templates.Created != 2 || summary.Strategies.Created != 1 {
		t.Fatalf("summary = %#v, want created templates and strategy", summary)
	}
	strategies, err := target.ListStrategies(ctx)
	if err != nil {
		t.Fatalf("list strategies: %v", err)
	}
	if len(strategies) != 1 {
		t.Fatalf("strategies len = %d, want 1", len(strategies))
	}
	if got := strategies[0].Templates; len(got) != 2 || got[0].ID != secondID || got[1].ID != firstID {
		t.Fatalf("imported strategy templates = %#v, want ordered ids", got)
	}
}

func TestJobStoreImportReusableConfigCollisionModes(t *testing.T) {
	ctx := context.Background()
	cfg := ReusableConfigExport{
		Version: ConfigExportVersion,
		Source:  "test",
		Templates: []ReusableConfigTemplate{{
			ID:         "tpl_collision",
			Name:       "incoming template",
			ParamsJSON: `{"Queries":["incoming"]}`,
		}},
		Strategies: []ReusableConfigStrategy{{
			ID:          "str_collision",
			Name:        "incoming strategy",
			TemplateIDs: []string{"tpl_collision"},
		}},
	}

	t.Run("rename", func(t *testing.T) {
		store := newTestJobStore(t)
		if _, err := store.SaveJobTemplateJSON(ctx, "tpl_collision", "existing template", `{"Queries":["existing"]}`); err != nil {
			t.Fatalf("save existing template: %v", err)
		}
		if _, err := store.SaveStrategy(ctx, "str_collision", "existing strategy", "", []string{"tpl_collision"}); err != nil {
			t.Fatalf("save existing strategy: %v", err)
		}
		summary, err := store.ImportReusableConfig(ctx, cfg, ConfigImportRename)
		if err != nil {
			t.Fatalf("rename import: %v", err)
		}
		if summary.Templates.Renamed != 1 || summary.Strategies.Renamed != 1 {
			t.Fatalf("summary = %#v, want renamed template and strategy", summary)
		}
		templates, err := store.ListJobTemplates(ctx)
		if err != nil {
			t.Fatalf("list templates: %v", err)
		}
		strategies, err := store.ListStrategies(ctx)
		if err != nil {
			t.Fatalf("list strategies: %v", err)
		}
		if len(templates) != 2 || len(strategies) != 2 {
			t.Fatalf("counts = %d templates/%d strategies, want 2/2", len(templates), len(strategies))
		}
	})

	t.Run("skip", func(t *testing.T) {
		store := newTestJobStore(t)
		if _, err := store.SaveJobTemplateJSON(ctx, "tpl_collision", "existing template", `{"Queries":["existing"]}`); err != nil {
			t.Fatalf("save existing template: %v", err)
		}
		if _, err := store.SaveStrategy(ctx, "str_collision", "existing strategy", "", []string{"tpl_collision"}); err != nil {
			t.Fatalf("save existing strategy: %v", err)
		}
		summary, err := store.ImportReusableConfig(ctx, cfg, ConfigImportSkip)
		if err != nil {
			t.Fatalf("skip import: %v", err)
		}
		if summary.Templates.Skipped != 1 || summary.Strategies.Skipped != 1 {
			t.Fatalf("summary = %#v, want skipped template and strategy", summary)
		}
		templates, _ := store.ListJobTemplates(ctx)
		strategies, _ := store.ListStrategies(ctx)
		if len(templates) != 1 || len(strategies) != 1 {
			t.Fatalf("counts = %d templates/%d strategies, want 1/1", len(templates), len(strategies))
		}
	})

	t.Run("overwrite", func(t *testing.T) {
		store := newTestJobStore(t)
		if _, err := store.SaveJobTemplateJSON(ctx, "tpl_collision", "existing template", `{"Queries":["existing"]}`); err != nil {
			t.Fatalf("save existing template: %v", err)
		}
		if _, err := store.SaveStrategy(ctx, "str_collision", "existing strategy", "", []string{"tpl_collision"}); err != nil {
			t.Fatalf("save existing strategy: %v", err)
		}
		summary, err := store.ImportReusableConfig(ctx, cfg, ConfigImportOverwrite)
		if err != nil {
			t.Fatalf("overwrite import: %v", err)
		}
		if summary.Templates.Updated != 1 || summary.Strategies.Updated != 1 {
			t.Fatalf("summary = %#v, want updated template and strategy", summary)
		}
		tpl, err := store.GetJobTemplate(ctx, "tpl_collision")
		if err != nil {
			t.Fatalf("get template: %v", err)
		}
		if tpl.Name != "incoming template" || tpl.ParamsJSON != `{"Queries":["incoming"]}` {
			t.Fatalf("template = %#v, want overwritten incoming values", tpl)
		}
	})

	t.Run("duplicate", func(t *testing.T) {
		store := newTestJobStore(t)
		summary, err := store.ImportReusableConfig(ctx, cfg, ConfigImportDuplicate)
		if err != nil {
			t.Fatalf("duplicate import: %v", err)
		}
		if summary.Templates.Duplicated != 1 || summary.Strategies.Duplicated != 1 {
			t.Fatalf("summary = %#v, want duplicated template and strategy", summary)
		}
		templates, _ := store.ListJobTemplates(ctx)
		strategies, _ := store.ListStrategies(ctx)
		if len(templates) != 1 || len(strategies) != 1 {
			t.Fatalf("counts = %d templates/%d strategies, want 1/1", len(templates), len(strategies))
		}
		if templates[0].ID == "tpl_collision" || strategies[0].ID == "str_collision" {
			t.Fatalf("duplicate kept original ids: templates=%#v strategies=%#v", templates, strategies)
		}
	})
}

func TestJobStoreImportReusableConfigValidationIsTransactional(t *testing.T) {
	ctx := context.Background()
	store := newTestJobStore(t)
	if _, err := store.SaveJobTemplateJSON(ctx, "tpl_existing", "existing", `{"Queries":["existing"]}`); err != nil {
		t.Fatalf("save existing template: %v", err)
	}
	cfg := ReusableConfigExport{
		Version: ConfigExportVersion,
		Templates: []ReusableConfigTemplate{{
			ID:         "tpl_new",
			Name:       "new",
			ParamsJSON: `{"Queries":["new"]}`,
		}},
		Strategies: []ReusableConfigStrategy{{
			ID:          "str_bad",
			Name:        "bad",
			TemplateIDs: []string{"tpl_missing"},
		}},
	}
	if _, err := store.ImportReusableConfig(ctx, cfg, ConfigImportRename); !errors.Is(err, ErrConfigImportInvalid) {
		t.Fatalf("import err = %v, want invalid", err)
	}
	templates, err := store.ListJobTemplates(ctx)
	if err != nil {
		t.Fatalf("list templates: %v", err)
	}
	if len(templates) != 1 || templates[0].ID != "tpl_existing" {
		t.Fatalf("templates after failed import = %#v, want only existing", templates)
	}
}

func TestJobStoreMigratesExistingSQLiteSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE jobs (
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
	)`); err != nil {
		t.Fatalf("create old jobs: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE job_templates (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		params_json TEXT NOT NULL,
		created_at DATETIME NOT NULL,
		last_used_at DATETIME NOT NULL
	)`); err != nil {
		t.Fatalf("create old templates: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw sqlite: %v", err)
	}

	store, err := OpenJobStore(path)
	if err != nil {
		t.Fatalf("open migrated store: %v", err)
	}
	defer store.Close()

	for _, column := range []string{"template_id", "strategy_id", "strategy_run_id"} {
		if !testColumnExists(t, store.db, "jobs", column) {
			t.Fatalf("jobs.%s was not migrated", column)
		}
	}
	for _, table := range []string{"strategies", "strategy_templates", "job_execution_stats"} {
		if !testTableExists(t, store.db, table) {
			t.Fatalf("%s table was not created", table)
		}
	}
}

func TestJobStoreStrategiesAndExecutionStats(t *testing.T) {
	ctx := context.Background()
	store := newTestJobStore(t)
	params := templateParamsForTest{Queries: []string{"coffee"}, Lang: "en", OutputMode: "file", JSONOut: true}
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	tplID, err := store.SaveJobTemplateJSON(ctx, "", "coffee template", string(paramsJSON))
	if err != nil {
		t.Fatalf("save template json: %v", err)
	}
	strategyID, err := store.SaveStrategy(ctx, "", "daily scrape", "morning batch", []string{tplID})
	if err != nil {
		t.Fatalf("save strategy: %v", err)
	}
	strategy, err := store.GetStrategy(ctx, strategyID)
	if err != nil {
		t.Fatalf("get strategy: %v", err)
	}
	if len(strategy.Templates) != 1 || strategy.Templates[0].ID != tplID {
		t.Fatalf("strategy templates = %#v, want %q", strategy.Templates, tplID)
	}

	jobID, err := store.CreateStartingJobWithSource(ctx, []string{"coffee"}, map[string]string{"lang": "en"}, tplID, strategyID, "run_1")
	if err != nil {
		t.Fatalf("create sourced job: %v", err)
	}
	if err := store.SetJobDiscoveryStats(ctx, jobID, 7, 2, 5); err != nil {
		t.Fatalf("set discovery stats: %v", err)
	}
	if err := store.IncrementJobStat(ctx, jobID, "scraped_urls", 3); err != nil {
		t.Fatalf("inc scraped: %v", err)
	}
	if err := store.IncrementJobStat(ctx, jobID, "duplicate_places", 1); err != nil {
		t.Fatalf("inc duplicate places: %v", err)
	}
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.TemplateID.String != tplID || job.StrategyID.String != strategyID || job.StrategyRunID.String != "run_1" {
		t.Fatalf("job source = %q/%q/%q", job.TemplateID.String, job.StrategyID.String, job.StrategyRunID.String)
	}
	if job.ExecutionStats.FeedURLsFound != 7 || job.ExecutionStats.FeedDuplicateURLs != 2 || job.ExecutionStats.QueuedURLs != 5 {
		t.Fatalf("discovery stats = %#v", job.ExecutionStats)
	}
	if job.ExecutionStats.ScrapedURLs != 3 || job.ExecutionStats.DuplicatePlaces != 1 {
		t.Fatalf("execution stats = %#v", job.ExecutionStats)
	}
}

type templateParamsForTest struct {
	Queries    []string `json:"Queries"`
	Lang       string   `json:"Lang,omitempty"`
	OutputMode string   `json:"OutputMode,omitempty"`
	JSONOut    bool     `json:"JSONOut,omitempty"`
}

func testColumnExists(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatalf("table info %s: %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			t.Fatalf("scan table info: %v", err)
		}
		if name == column {
			return true
		}
	}
	return false
}

func testTableExists(t *testing.T, db *sql.DB, table string) bool {
	t.Helper()
	var name string
	err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	if err != nil {
		t.Fatalf("table exists %s: %v", table, err)
	}
	return name == table
}

func TestJobStoreQueueStartingJobURLs(t *testing.T) {
	ctx := context.Background()
	store := newTestJobStore(t)
	jobID, err := store.CreateStartingJob(ctx, []string{"coffee"}, nil)
	if err != nil {
		t.Fatalf("create starting job: %v", err)
	}
	if err := store.QueueStartingJobURLs(ctx, jobID, []string{"u1", "u2"}); err != nil {
		t.Fatalf("queue URLs: %v", err)
	}
	stats, err := store.JobStats(ctx, jobID)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Pending != 2 || stats.Total != 2 {
		t.Fatalf("pending/total = %d/%d, want 2/2", stats.Pending, stats.Total)
	}
	if err := store.StartJob(ctx, jobID); err != nil {
		t.Fatalf("start job: %v", err)
	}
	if err := store.QueueStartingJobURLs(ctx, jobID, []string{"u3"}); err == nil {
		t.Fatal("expected queueing a non-starting job to fail")
	}
}

func TestJobStoreClaimResumeRejectsStartingJob(t *testing.T) {
	ctx := context.Background()
	store := newTestJobStore(t)
	jobID, err := store.CreateStartingJob(ctx, []string{"coffee"}, nil)
	if err != nil {
		t.Fatalf("create starting job: %v", err)
	}
	if _, err := store.ClaimResume(ctx, jobID); !errors.Is(err, ErrJobNotResumable) {
		t.Fatalf("claim starting job error = %v, want ErrJobNotResumable", err)
	}
}
