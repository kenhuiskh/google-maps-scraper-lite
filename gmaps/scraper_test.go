package gmaps

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// captureLog redirects the global logger into a buffer for the test's
// duration. The buffer must only be read after all Run goroutines have
// stopped (Run waits for its monitor before returning).
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

type fakePagePool struct{}

func (fakePagePool) AcquirePage(ctx context.Context) (Page, error) { return nil, nil }
func (fakePagePool) ReleasePage(page Page)                         {}

type trackingFeedPagePool struct {
	page     Page
	released int
	retired  int
}

func (p *trackingFeedPagePool) AcquirePage(context.Context) (Page, error) { return p.page, nil }
func (p *trackingFeedPagePool) ReleasePage(Page)                          { p.released++ }
func (p *trackingFeedPagePool) RetirePage(Page)                           { p.retired++ }

func TestEnsureJobRerunsInterruptedDiscoveryBeforeStartingWorkers(t *testing.T) {
	ctx := context.Background()
	store := newTestJobStore(t)
	jobID, err := store.CreateStartingJob(ctx, []string{"coffee"}, nil)
	if err != nil {
		t.Fatalf("create starting job: %v", err)
	}
	if err := store.RecoverStaleActiveJob(ctx, jobID, errors.New("process restarted; job interrupted")); err != nil {
		t.Fatalf("recover interrupted discovery: %v", err)
	}
	if _, err := store.ClaimResume(ctx, jobID); err != nil {
		t.Fatalf("claim resume: %v", err)
	}

	const placeURL = "https://www.google.com/maps/place/Coffee/data=!4m2!3m1!1s0x1:0x2"
	var feedCalls int
	s := Scraper{
		Config: Config{JobID: jobID},
		Pool:   fakePagePool{},
		Store:  store,
		ScrapeFeed: func(context.Context, Page, string, FeedOptions) ([]string, error) {
			feedCalls++
			return []string{placeURL}, nil
		},
	}
	if _, err := s.ensureJob(ctx, []string{"coffee"}, FeedOptions{LangCode: "en"}, []string{"en"}); err != nil {
		t.Fatalf("ensure job: %v", err)
	}
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("get resumed job: %v", err)
	}
	if feedCalls != 1 || job.Status != JobStatusRunning || job.Stats.Pending != 1 {
		t.Fatalf("resumed discovery calls/status/pending = %d/%s/%d, want 1/running/1", feedCalls, job.Status, job.Stats.Pending)
	}
}

func TestCollectPlaceURLsRetiresPageAfterFeedError(t *testing.T) {
	pool := &trackingFeedPagePool{page: &reviewTagsTestPage{}}
	s := Scraper{
		Pool: pool,
		ScrapeFeed: func(context.Context, Page, string, FeedOptions) ([]string, error) {
			return nil, context.DeadlineExceeded
		},
	}

	if _, err := s.collectPlaceURLs(context.Background(), []string{"bars"}, FeedOptions{LangCode: "en"}, "", []string{"en"}); err != nil {
		t.Fatalf("collectPlaceURLs: %v", err)
	}
	if pool.retired != 1 || pool.released != 0 {
		t.Fatalf("pool retired/released = %d/%d, want 1/0", pool.retired, pool.released)
	}
}

func TestCollectPlaceURLsResolvesPlaceIDsThroughFeed(t *testing.T) {
	ctx := context.Background()
	var gotQueries []string
	s := Scraper{
		Pool: fakePagePool{},
		ScrapeFeed: func(ctx context.Context, page Page, query string, opts FeedOptions) ([]string, error) {
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
		ScrapeFeed: func(ctx context.Context, page Page, query string, opts FeedOptions) ([]string, error) {
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
		ScrapeFeed: func(ctx context.Context, page Page, query string, opts FeedOptions) ([]string, error) {
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
		ScrapePlace: func(ctx context.Context, page Page, placeURL string, opts PlaceOptions) (*Entry, error) {
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
		ScrapePlace: func(ctx context.Context, page Page, placeURL string, opts PlaceOptions) (*Entry, error) {
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

func TestScraperAutoRecoverStopsAtMaxURLAttempts(t *testing.T) {
	ctx := context.Background()
	store := newTestJobStore(t)
	jobID, err := store.CreateJob(ctx, []string{"coffee"}, nil, URLsNoLang([]string{"u1", "u2"}))
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	attempts := make(map[string]int)
	s := Scraper{
		Config: Config{
			Concurrency:      1,
			JobID:            jobID,
			AutoRecover:      true,
			MaxURLAttempts:   2,
			RecoveryMinDelay: time.Millisecond,
			RecoveryMaxDelay: time.Millisecond,
			BrowseStartDelay: time.Millisecond,
		},
		Pool:  fakePagePool{},
		Store: store,
		ScrapePlace: func(ctx context.Context, page Page, placeURL string, opts PlaceOptions) (*Entry, error) {
			attempts[placeURL]++
			if placeURL == "u1" {
				return nil, errors.New("permanent parse failure")
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
		t.Fatalf("attempts = %#v, want u1 twice and u2 once", attempts)
	}
	stats, err := store.JobStats(ctx, jobID)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Done != 1 || stats.Failed != 1 || stats.Pending != 0 {
		t.Fatalf("stats = %+v, want 1 done / 1 failed / 0 pending", stats)
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
		ScrapePlace: func(ctx context.Context, page Page, placeURL string, opts PlaceOptions) (*Entry, error) {
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
		ScrapePlace: func(ctx context.Context, page Page, placeURL string, opts PlaceOptions) (*Entry, error) {
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

func TestScrapeDeadlineIsNotTransient(t *testing.T) {
	if isTransientNavError(ErrScrapeDeadline) {
		t.Fatal("ErrScrapeDeadline must not qualify for a same-page fast retry: the watchdog closed the page")
	}
}

func TestScraperWatchdogRequeuesStuckScrape(t *testing.T) {
	ctx := context.Background()
	store := newTestJobStore(t)
	jobID, err := store.CreateJob(ctx, []string{"coffee"}, nil, URLsNoLang([]string{"u1", "u2"}))
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	block := make(chan struct{})
	defer close(block)
	var mu sync.Mutex
	attempts := make(map[string]int)
	s := Scraper{
		Config: Config{
			Concurrency:      1,
			JobID:            jobID,
			AutoRecover:      true,
			RecoveryMinDelay: time.Millisecond,
			RecoveryMaxDelay: time.Millisecond,
			BrowseStartDelay: time.Millisecond,
			ScrapeDeadline:   50 * time.Millisecond,
		},
		Pool:  fakePagePool{},
		Store: store,
		ScrapePlace: func(ctx context.Context, page Page, placeURL string, opts PlaceOptions) (*Entry, error) {
			mu.Lock()
			attempts[placeURL]++
			n := attempts[placeURL]
			mu.Unlock()
			if placeURL == "u1" && n == 1 {
				<-block // wedged page-driver call: never returns on its own
				return nil, errors.New("unblocked by test teardown")
			}
			return &Entry{PlaceID: placeURL}, nil
		},
	}

	out := make(chan PlaceResult, 4)
	errCh := make(chan error, 1)
	go func() {
		errCh <- s.Run(ctx, nil, out)
	}()
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for result := range out {
			if err := store.MarkURLDone(ctx, result.URLID); err != nil {
				t.Errorf("mark done: %v", err)
			}
		}
	}()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("run error = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not terminate: watchdog failed to fire on stuck scrape")
	}
	<-drained
	mu.Lock()
	gotU1, gotU2 := attempts["u1"], attempts["u2"]
	mu.Unlock()
	if gotU1 != 2 || gotU2 != 1 {
		t.Fatalf("attempts = u1:%d u2:%d, want u1 requeued once and u2 once", gotU1, gotU2)
	}
	stats, err := store.JobStats(ctx, jobID)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Done != 2 || stats.Pending != 0 || stats.Failed != 0 {
		t.Fatalf("stats = %+v, want 2 done / 0 pending / 0 failed", stats)
	}
	es, err := store.JobExecutionStats(ctx, jobID)
	if err != nil {
		t.Fatalf("exec stats: %v", err)
	}
	if es.RetryEvents < 1 {
		t.Fatalf("RetryEvents = %d, want >= 1 (watchdog requeue)", es.RetryEvents)
	}
}

func TestScraperMonitorHeartbeat(t *testing.T) {
	ctx := context.Background()
	store := newTestJobStore(t)
	jobID, err := store.CreateJob(ctx, []string{"coffee"}, nil, URLsNoLang([]string{"u1", "u2", "u3"}))
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	buf := captureLog(t)

	s := Scraper{
		Config: Config{
			Concurrency:       1,
			JobID:             jobID,
			BrowseStartDelay:  time.Millisecond,
			HeartbeatInterval: 20 * time.Millisecond,
		},
		Pool:  fakePagePool{},
		Store: store,
		ScrapePlace: func(ctx context.Context, page Page, placeURL string, opts PlaceOptions) (*Entry, error) {
			// Keep the run alive across several heartbeat ticks.
			time.Sleep(30 * time.Millisecond)
			return &Entry{PlaceID: placeURL}, nil
		},
	}

	out := make(chan PlaceResult, 4)
	errCh := make(chan error, 1)
	go func() {
		errCh <- s.Run(ctx, nil, out)
	}()
	for result := range out {
		if err := store.MarkURLDone(ctx, result.URLID); err != nil {
			t.Fatalf("mark done: %v", err)
		}
	}
	// Run blocks on the monitor's stopped channel, so a return here proves the
	// monitor goroutine exited with the workers.
	if err := <-errCh; err != nil {
		t.Fatalf("run error = %v, want nil", err)
	}
	logs := buf.String()
	if !strings.Contains(logs, "heartbeat: done=") {
		t.Fatalf("logs missing heartbeat line:\n%s", logs)
	}
	if !strings.Contains(logs, "inflight=") {
		t.Fatalf("logs missing inflight registry in heartbeat:\n%s", logs)
	}
	if strings.Contains(logs, "STALL DETECTED") {
		t.Fatalf("stall watchdog fired on a healthy run:\n%s", logs)
	}
}

func TestScraperStallWatchdogPausesJobAndExits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := newTestJobStore(t)
	jobID, err := store.CreateJob(ctx, []string{"coffee"}, nil, URLsNoLang([]string{"u1"}))
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	buf := captureLog(t)

	exitCode := make(chan int, 1)
	prevExit := stallExit
	stallExit = func(code int) { exitCode <- code }
	t.Cleanup(func() { stallExit = prevExit })

	block := make(chan struct{})
	defer close(block)

	s := Scraper{
		Config: Config{
			Concurrency:       1,
			JobID:             jobID,
			BrowseStartDelay:  time.Millisecond,
			ScrapeDeadline:    time.Hour, // per-scrape watchdog must not fire first
			HeartbeatInterval: 10 * time.Millisecond,
			StallTimeout:      50 * time.Millisecond,
		},
		Pool:  fakePagePool{},
		Store: store,
		ScrapePlace: func(ctx context.Context, page Page, placeURL string, opts PlaceOptions) (*Entry, error) {
			<-block // wedged claim: only the stall watchdog can react
			return nil, errors.New("unblocked by test teardown")
		},
	}

	out := make(chan PlaceResult, 2)
	errCh := make(chan error, 1)
	go func() {
		errCh <- s.Run(ctx, nil, out)
	}()
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for range out {
		}
	}()

	select {
	case code := <-exitCode:
		if code != ExitCodeStallWatchdog {
			t.Fatalf("exit code = %d, want %d", code, ExitCodeStallWatchdog)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("stall watchdog did not request an exit")
	}

	// Unwedge the run and wait for Run to return before reading shared state;
	// finishJob leaves the paused status alone on context.Canceled.
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("run error = %v, want nil", err)
	}
	<-drained

	job, err := store.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Status != JobStatusPaused {
		t.Fatalf("job status = %q, want %q", job.Status, JobStatusPaused)
	}
	logs := buf.String()
	if !strings.Contains(logs, "STALL DETECTED: no progress for") {
		t.Fatalf("logs missing stall line:\n%s", logs)
	}
	if !strings.Contains(logs, "u1") {
		t.Fatalf("logs missing in-flight URL in stall diagnostics:\n%s", logs)
	}
	if !strings.Contains(logs, "goroutine ") {
		t.Fatalf("logs missing goroutine stack dump:\n%s", logs)
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
			ScrapePlace: func(ctx context.Context, page Page, placeURL string, opts PlaceOptions) (*Entry, error) {
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
			ScrapePlace: func(ctx context.Context, page Page, placeURL string, opts PlaceOptions) (*Entry, error) {
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

func TestScraperHTTPFirstSkipsBrowser(t *testing.T) {
	ctx := context.Background()
	store := newTestJobStore(t)
	jobID, err := store.CreateJob(ctx, []string{"coffee"}, nil, URLsNoLang([]string{"u1"}))
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	var browserCalled atomic.Bool
	s := Scraper{
		Config: Config{Concurrency: 1, JobID: jobID, EnableHTTPFirst: true},
		Pool:   fakePagePool{},
		Store:  store,
		ScrapePlaceHTTP: func(ctx context.Context, placeURL string, opts PlaceOptions) (*Entry, error) {
			return &Entry{PlaceID: "http:" + placeURL}, nil
		},
		ScrapePlace: func(ctx context.Context, page Page, placeURL string, opts PlaceOptions) (*Entry, error) {
			browserCalled.Store(true)
			return nil, nil
		},
	}

	out := make(chan PlaceResult, 2)
	errCh := make(chan error, 1)
	go func() {
		errCh <- s.Run(ctx, nil, out)
	}()
	var results []PlaceResult
	for result := range out {
		results = append(results, result)
		if err := store.MarkURLDone(ctx, result.URLID); err != nil {
			t.Fatalf("mark done: %v", err)
		}
	}
	if err := <-errCh; err != nil {
		t.Fatalf("run error = %v, want nil", err)
	}
	if browserCalled.Load() {
		t.Errorf("browser ScrapePlace should not be called when HTTP succeeds")
	}
	if len(results) != 1 || results[0].Entry.PlaceID != "http:u1" {
		t.Fatalf("results = %#v, want single entry from HTTP stub", results)
	}
	stats, err := store.JobStats(ctx, jobID)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Done != 1 || stats.Failed != 0 {
		t.Fatalf("stats = %+v, want 1 done / 0 failed", stats)
	}
}

func TestScraperHTTPUnavailableFallsBackToBrowser(t *testing.T) {
	ctx := context.Background()
	store := newTestJobStore(t)
	jobID, err := store.CreateJob(ctx, []string{"coffee"}, nil, URLsNoLang([]string{"u1"}))
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	s := Scraper{
		Config: Config{Concurrency: 1, JobID: jobID, EnableHTTPFirst: true},
		Pool:   fakePagePool{},
		Store:  store,
		ScrapePlaceHTTP: func(ctx context.Context, placeURL string, opts PlaceOptions) (*Entry, error) {
			return nil, ErrHTTPPlaceUnavailable
		},
		ScrapePlace: func(ctx context.Context, page Page, placeURL string, opts PlaceOptions) (*Entry, error) {
			return &Entry{PlaceID: "browser:" + placeURL}, nil
		},
	}

	out := make(chan PlaceResult, 2)
	errCh := make(chan error, 1)
	go func() {
		errCh <- s.Run(ctx, nil, out)
	}()
	var results []PlaceResult
	for result := range out {
		results = append(results, result)
		if err := store.MarkURLDone(ctx, result.URLID); err != nil {
			t.Fatalf("mark done: %v", err)
		}
	}
	if err := <-errCh; err != nil {
		t.Fatalf("run error = %v, want nil", err)
	}
	if len(results) != 1 || results[0].Entry.PlaceID != "browser:u1" {
		t.Fatalf("results = %#v, want single entry from browser stub", results)
	}
	stats, err := store.JobStats(ctx, jobID)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Done != 1 || stats.Failed != 0 {
		t.Fatalf("stats = %+v, want 1 done / 0 failed", stats)
	}
}

func TestScraperHTTPBotBlockFallsBackToBrowser(t *testing.T) {
	ctx := context.Background()
	store := newTestJobStore(t)
	jobID, err := store.CreateJob(ctx, []string{"coffee"}, nil, URLsNoLang([]string{"u1"}))
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	s := Scraper{
		Config: Config{Concurrency: 1, JobID: jobID, EnableHTTPFirst: true},
		Pool:   fakePagePool{},
		Store:  store,
		ScrapePlaceHTTP: func(ctx context.Context, placeURL string, opts PlaceOptions) (*Entry, error) {
			return nil, fmt.Errorf("bot block: HTTP 429: %w", ErrBotBlocked)
		},
		ScrapePlace: func(ctx context.Context, page Page, placeURL string, opts PlaceOptions) (*Entry, error) {
			return &Entry{PlaceID: "browser:" + placeURL}, nil
		},
	}

	out := make(chan PlaceResult, 2)
	errCh := make(chan error, 1)
	go func() {
		errCh <- s.Run(ctx, nil, out)
	}()
	var results []PlaceResult
	for result := range out {
		results = append(results, result)
		if err := store.MarkURLDone(ctx, result.URLID); err != nil {
			t.Fatalf("mark done: %v", err)
		}
	}
	if err := <-errCh; err != nil {
		t.Fatalf("run error = %v, want nil", err)
	}
	if len(results) != 1 || results[0].Entry.PlaceID != "browser:u1" {
		t.Fatalf("results = %#v, want single entry from browser stub (HTTP bot-block must degrade, not fail)", results)
	}
	stats, err := store.JobStats(ctx, jobID)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Done != 1 || stats.Failed != 0 {
		t.Fatalf("stats = %+v, want 1 done / 0 failed (URL must not be failed by an HTTP-side bot block)", stats)
	}
}

func TestScraperExtraReviewsBypassesHTTP(t *testing.T) {
	ctx := context.Background()
	store := newTestJobStore(t)
	jobID, err := store.CreateJob(ctx, []string{"coffee"}, nil, URLsNoLang([]string{"u1"}))
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	var httpCalled atomic.Bool
	s := Scraper{
		Config: Config{Concurrency: 1, JobID: jobID, EnableHTTPFirst: true, ExtraReviews: 1},
		Pool:   fakePagePool{},
		Store:  store,
		ScrapePlaceHTTP: func(ctx context.Context, placeURL string, opts PlaceOptions) (*Entry, error) {
			httpCalled.Store(true)
			return nil, nil
		},
		ScrapePlace: func(ctx context.Context, page Page, placeURL string, opts PlaceOptions) (*Entry, error) {
			return &Entry{PlaceID: "browser:" + placeURL}, nil
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
	if httpCalled.Load() {
		t.Errorf("ScrapePlaceHTTP should not be called when ExtraReviews > 0")
	}
}

func TestScraperDefaultUsesBrowserNotHTTP(t *testing.T) {
	ctx := context.Background()
	store := newTestJobStore(t)
	jobID, err := store.CreateJob(ctx, []string{"coffee"}, nil, URLsNoLang([]string{"u1"}))
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	var httpCalled atomic.Bool
	s := Scraper{
		Config: Config{Concurrency: 1, JobID: jobID},
		Pool:   fakePagePool{},
		Store:  store,
		ScrapePlaceHTTP: func(ctx context.Context, placeURL string, opts PlaceOptions) (*Entry, error) {
			httpCalled.Store(true)
			return nil, nil
		},
		ScrapePlace: func(ctx context.Context, page Page, placeURL string, opts PlaceOptions) (*Entry, error) {
			return &Entry{PlaceID: "browser:" + placeURL}, nil
		},
	}

	out := make(chan PlaceResult, 2)
	errCh := make(chan error, 1)
	go func() {
		errCh <- s.Run(ctx, nil, out)
	}()
	var results []PlaceResult
	for result := range out {
		results = append(results, result)
		if err := store.MarkURLDone(ctx, result.URLID); err != nil {
			t.Fatalf("mark done: %v", err)
		}
	}
	if err := <-errCh; err != nil {
		t.Fatalf("run error = %v, want nil", err)
	}
	if httpCalled.Load() {
		t.Errorf("ScrapePlaceHTTP should not be called when EnableHTTPFirst is unset (default off)")
	}
	if len(results) != 1 || results[0].Entry.PlaceID != "browser:u1" {
		t.Fatalf("results = %#v, want single entry from browser stub", results)
	}
}
