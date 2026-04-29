package gmaps

import (
	"context"
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
