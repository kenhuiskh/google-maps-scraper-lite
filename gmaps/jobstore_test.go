package gmaps

import (
	"context"
	"database/sql"
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
