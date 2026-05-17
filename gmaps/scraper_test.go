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

	collected := s.collectPlaceURLs(ctx, []string{"place_id:ChIJ123", "coffee"}, FeedOptions{LangCode: "en"})
	if len(gotQueries) != 2 || gotQueries[0] != "place_id:ChIJ123" || gotQueries[1] != "coffee" {
		t.Fatalf("queries = %#v, want place_id then coffee", gotQueries)
	}
	if len(collected.URLs) != 2 {
		t.Fatalf("urls = %#v, want 2", collected.URLs)
	}
	if collected.URLs[0] == PlaceIDToURL("ChIJ123") {
		t.Fatalf("place ID queued synthetic URL %q", collected.URLs[0])
	}
	if collected.URLs[0] != "https://www.google.com/maps/place/Test/data=!4m2!3m1!1s0x1:0x2" {
		t.Fatalf("first url = %q, want canonical feed URL", collected.URLs[0])
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

	collected := s.collectPlaceURLs(ctx, []string{"place_id:ChIJ123", "coffee"}, FeedOptions{LangCode: "en"})
	want := PlaceIDToURL("ChIJ123")
	if len(collected.URLs) != 1 || collected.URLs[0] != want {
		t.Fatalf("urls = %#v, want only %q", collected.URLs, want)
	}
}

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
