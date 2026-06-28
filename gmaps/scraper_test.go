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

func TestCollectPlaceURLsResolvesPlaceIDsThroughFeed(t *testing.T) {
	ctx := context.Background()
	var gotQueries []string
	s := Scraper{
		Pool: fakePagePool{},
		ScrapeFeed: func(ctx context.Context, page playwright.Page, query string, opts FeedOptions) ([]string, error) {
			gotQueries = append(gotQueries, query)
			switch query {
			case "place_id:ChIJ123":
				return []string{"https://www.google.com/maps/place/Test/data=!4m2!3m1!1s0x1:0x2"}, nil
			case "coffee":
				return []string{"https://www.google.com/maps/place/Coffee/data=!4m2!3m1!1s0x3:0x4"}, nil
			default:
				t.Fatalf("unexpected query %q", query)
				return nil, nil
			}
		},
	}

	collected, err := s.collectPlaceURLs(ctx, []string{"place_id:ChIJ123", "coffee"}, FeedOptions{LangCode: "en"}, "", []string{"en"})
	if err != nil {
		t.Fatalf("collectPlaceURLs: %v", err)
	}
	if len(gotQueries) != 2 || gotQueries[0] != "place_id:ChIJ123" || gotQueries[1] != "coffee" {
		t.Fatalf("queries = %#v, want place_id then coffee", gotQueries)
	}
	if len(collected.URLs) != 2 {
		t.Fatalf("urls = %#v, want 2", collected.URLs)
	}
	if collected.URLs[0].URL == PlaceIDToURL("ChIJ123") {
		t.Fatalf("place ID queued synthetic URL %q", collected.URLs[0].URL)
	}
	if collected.URLs[0].URL != "https://www.google.com/maps/place/Test/data=!4m2!3m1!1s0x1:0x2" {
		t.Fatalf("first url = %q, want canonical feed URL", collected.URLs[0].URL)
	}
	if collected.URLs[0].Lang != "en" {
		t.Fatalf("first url lang = %q, want en", collected.URLs[0].Lang)
	}
	if collected.FeedURLsFound != 2 {
		t.Fatalf("FeedURLsFound = %d, want 2", collected.FeedURLsFound)
	}
}

func TestCollectPlaceURLsFallsBackForPlaceIDFeedError(t *testing.T) {
	ctx := context.Background()
	s := Scraper{
		Pool: fakePagePool{},
		ScrapeFeed: func(ctx context.Context, page playwright.Page, query string, opts FeedOptions) ([]string, error) {
			return nil, errors.New("feed selector timeout")
		},
	}

	collected, err := s.collectPlaceURLs(ctx, []string{"place_id:ChIJ123", "coffee"}, FeedOptions{LangCode: "en"}, "", []string{"en"})
	if err != nil {
		t.Fatalf("collectPlaceURLs: %v", err)
	}
	want := PlaceIDToURL("ChIJ123")
	if len(collected.URLs) != 1 || collected.URLs[0].URL != want {
		t.Fatalf("urls = %#v, want only %q", collected.URLs, want)
	}
}

func TestCollectPlaceURLsSkipsAlreadyScraped(t *testing.T) {
	ctx := context.Background()
	store := newTestJobStore(t)

	const seededURL = "https://www.google.com/maps/place/Old/data=!4m2!3m1!1s0x1:0x2"
	const newURL = "https://www.google.com/maps/place/New/data=!4m2!3m1!1s0x3:0x4"

	// Seed seededURL as done in a prior job.
	jobID, err := store.CreateJob(ctx, []string{"seed"}, nil, URLsNoLang([]string{seededURL}))
	if err != nil {
		t.Fatalf("create seed job: %v", err)
	}
	if err := store.StartJob(ctx, jobID); err != nil {
		t.Fatalf("start seed job: %v", err)
	}
	claimed, err := store.ClaimNextURL(ctx, jobID)
	if err != nil {
		t.Fatalf("claim seed url: %v", err)
	}
	if err := store.MarkURLDone(ctx, claimed.ID); err != nil {
		t.Fatalf("mark seed url done: %v", err)
	}

	s := Scraper{
		Pool:  fakePagePool{},
		Store: store,
		ScrapeFeed: func(ctx context.Context, page playwright.Page, query string, opts FeedOptions) ([]string, error) {
			return []string{seededURL, newURL}, nil
		},
		Config: Config{DedupScope: "all"},
	}

	collected, err := s.collectPlaceURLs(ctx, []string{"coffee"}, FeedOptions{LangCode: "en"}, "", []string{"en"})
	if err != nil {
		t.Fatalf("collectPlaceURLs: %v", err)
	}
	if len(collected.URLs) != 1 || collected.URLs[0].URL != newURL {
		t.Fatalf("urls = %#v, want only %q", collected.URLs, newURL)
	}
	if collected.CrossJobDuplicateURLs != 1 {
		t.Fatalf("CrossJobDuplicateURLs = %d, want 1", collected.CrossJobDuplicateURLs)
	}
}

func TestScraperGracefulPauseFinishesCurrentURL(t *testing.T) {
	ctx := context.Background()
	store := newTestJobStore(t)
	urls := make([]string, 10)
	for i := range urls {
		urls[i] = fmt.Sprintf("u%d", i+1)
	}
	jobID, err := store.CreateJob(ctx, []string{"coffee"}, nil, URLsNoLang(urls))
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
	jobID, err := store.CreateJob(ctx, []string{"coffee"}, nil, URLsNoLang(urls))
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

func TestScraperTransientNavErrorRetriesNoRecovery(t *testing.T) {
	ctx := context.Background()
	store := newTestJobStore(t)
	jobID, err := store.CreateJob(ctx, []string{"coffee"}, nil, URLsNoLang([]string{"u1"}))
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	attempts := 0
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
			attempts++
			if attempts == 1 {
				return nil, errors.New("Frame.Goto: Timeout 30000ms exceeded")
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
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2 (in-process retry)", attempts)
	}
	stats, err := store.JobStats(ctx, jobID)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Done != 1 || stats.Pending != 0 || stats.Failed != 0 {
		t.Fatalf("stats = %+v, want 1 done / 0 pending / 0 failed", stats)
	}
	// Transient retry handled on the same claim: no requeue should have occurred.
	es, err := store.JobExecutionStats(ctx, jobID)
	if err != nil {
		t.Fatalf("exec stats: %v", err)
	}
	if es.RetryEvents != 0 {
		t.Fatalf("RetryEvents = %d, want 0 (no requeue/recovery)", es.RetryEvents)
	}
}

func TestScraperBotBlockTriggersRecovery(t *testing.T) {
	ctx := context.Background()
	store := newTestJobStore(t)
	jobID, err := store.CreateJob(ctx, []string{"coffee"}, nil, URLsNoLang([]string{"u1", "u2"}))
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	attempts := make(map[string]int)
	firstBlocked := false
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
			if !firstBlocked {
				firstBlocked = true
				return nil, fmt.Errorf("bot block: /sorry/: %w", ErrBotBlocked)
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
	stats, err := store.JobStats(ctx, jobID)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Done != 2 || stats.Failed != 0 {
		t.Fatalf("stats = %+v, want 2 done / 0 failed", stats)
	}
	es, err := store.JobExecutionStats(ctx, jobID)
	if err != nil {
		t.Fatalf("exec stats: %v", err)
	}
	if es.RetryEvents < 1 {
		t.Fatalf("RetryEvents = %d, want >= 1 (bot-block requeue)", es.RetryEvents)
	}
}

func TestScraperConsecutiveFailuresBackstop(t *testing.T) {
	t.Run("autoRecoverOff_returnsSessionBlocked", func(t *testing.T) {
		ctx := context.Background()
		store := newTestJobStore(t)
		urls := make([]string, 10)
		for i := range urls {
			urls[i] = fmt.Sprintf("u%d", i+1)
		}
		jobID, err := store.CreateJob(ctx, []string{"coffee"}, nil, URLsNoLang(urls))
		if err != nil {
			t.Fatalf("create job: %v", err)
		}

		s := Scraper{
			Config: Config{Concurrency: 1, JobID: jobID},
			Pool:   fakePagePool{},
			Store:  store,
			ScrapePlace: func(ctx context.Context, page playwright.Page, placeURL string, opts PlaceOptions) (*Entry, error) {
				return nil, errors.New("parse entry JSON: boom")
			},
		}

		out := make(chan PlaceResult, 2)
		errCh := make(chan error, 1)
		go func() {
			errCh <- s.Run(ctx, nil, out)
		}()
		for range out {
		}
		if err := <-errCh; !errors.Is(err, ErrSessionBlocked) {
			t.Fatalf("run error = %v, want ErrSessionBlocked", err)
		}
	})

	t.Run("autoRecoverOn_backstopTriggersRecovery", func(t *testing.T) {
		ctx := context.Background()
		store := newTestJobStore(t)
		jobID, err := store.CreateJob(ctx, []string{"coffee"}, nil, URLsNoLang([]string{"u1"}))
		if err != nil {
			t.Fatalf("create job: %v", err)
		}

		attempts := 0
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
				attempts++
				if attempts <= int(blockThreshold) {
					return nil, errors.New("parse entry JSON: boom")
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
		if attempts != int(blockThreshold)+1 {
			t.Fatalf("attempts = %d, want %d", attempts, blockThreshold+1)
		}
		stats, err := store.JobStats(ctx, jobID)
		if err != nil {
			t.Fatalf("stats: %v", err)
		}
		if stats.Done != 1 {
			t.Fatalf("stats = %+v, want 1 done", stats)
		}
		es, err := store.JobExecutionStats(ctx, jobID)
		if err != nil {
			t.Fatalf("exec stats: %v", err)
		}
		if es.RetryEvents < int(blockThreshold) {
			t.Fatalf("RetryEvents = %d, want >= %d", es.RetryEvents, blockThreshold)
		}
	})
}
