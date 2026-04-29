package gmaps

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/playwright-community/playwright-go"
)

type fakePagePool struct{}

func (fakePagePool) AcquirePage() playwright.Page     { return nil }
func (fakePagePool) ReleasePage(page playwright.Page) {}

func TestScraperGracefulPauseFinishesCurrentURL(t *testing.T) {
	ctx := context.Background()
	store := newTestJobStore(t)
	urls := make([]string, 10)
	for i := range urls {
		urls[i] = fmt.Sprintf("u%d", i+1)
	}
	jobID, err := store.CreateJob(ctx, []string{"coffee"}, nil, urls)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	var seen int
	s := Scraper{
		Config: Config{Concurrency: 1, JobID: jobID},
		Pool:   fakePagePool{},
		Store:  store,
		ScrapePlace: func(ctx context.Context, page playwright.Page, placeURL string, opts PlaceOptions) (*Entry, error) {
			seen++
			if seen == 7 {
				if err := store.RequestPause(context.Background(), jobID); err != nil {
					t.Fatalf("request pause: %v", err)
				}
			}
			return &Entry{PlaceID: placeURL}, nil
		},
	}

	out := make(chan PlaceResult, 2)
	errCh := make(chan error, 1)
	go func() {
		errCh <- s.Run(ctx, nil, out)
	}()
	for result := range out {
		if err := store.MarkURLDone(ctx, result.URLID); err != nil {
			t.Fatalf("mark done: %v", err)
		}
	}
	if err := <-errCh; err != ErrJobPaused {
		t.Fatalf("run error = %v, want ErrJobPaused", err)
	}
	stats, err := store.JobStats(ctx, jobID)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Done != 7 || stats.Pending != 3 {
		t.Fatalf("done=%d pending=%d, want 7/3", stats.Done, stats.Pending)
	}
}

func TestScraperAutoRecoverRequeuesFailedURL(t *testing.T) {
	ctx := context.Background()
	store := newTestJobStore(t)
	urls := []string{"u1", "u2"}
	jobID, err := store.CreateJob(ctx, []string{"coffee"}, nil, urls)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	attempts := make(map[string]int)
	s := Scraper{
		Config: Config{
			Concurrency:      1,
			JobID:            jobID,
			AutoRecover:      true,
			RecoveryMinDelay: time.Millisecond,
			RecoveryMaxDelay: time.Millisecond,
			BrowseStartDelay: time.Millisecond,
		},
		Pool:  fakePagePool{},
		Store: store,
		ScrapePlace: func(ctx context.Context, page playwright.Page, placeURL string, opts PlaceOptions) (*Entry, error) {
			attempts[placeURL]++
			if placeURL == "u1" && attempts[placeURL] == 1 {
				return nil, errors.New("temporary tab error")
			}
			return &Entry{PlaceID: placeURL}, nil
		},
	}

	out := make(chan PlaceResult, 2)
	errCh := make(chan error, 1)
	go func() {
		errCh <- s.Run(ctx, nil, out)
	}()
	for result := range out {
		if err := store.MarkURLDone(ctx, result.URLID); err != nil {
			t.Fatalf("mark done: %v", err)
		}
	}
	if err := <-errCh; err != nil {
		t.Fatalf("run error = %v, want nil", err)
	}
	if attempts["u1"] != 2 || attempts["u2"] != 1 {
		t.Fatalf("attempts = %#v, want u1 retried once and u2 once", attempts)
	}
	stats, err := store.JobStats(ctx, jobID)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Done != 2 || stats.Pending != 0 || stats.Failed != 0 {
		t.Fatalf("stats = %+v, want all done without failed URLs", stats)
	}
}
