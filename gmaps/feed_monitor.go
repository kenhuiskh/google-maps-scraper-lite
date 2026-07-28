package gmaps

import (
	"context"
	"fmt"
	"log"
	"runtime"
	"sync"
	"time"
)

const (
	feedStageInitializing = "initializing"
	feedStageAcquirePage  = "acquire_page"
	feedStageScrapeFeed   = "scrape_feed"
	feedStageNavigate     = "navigate"
	feedStageConsent      = "consent"
	feedStageWaitFeed     = "wait_feed"
	feedStageScrollFeed   = "scroll_feed"
	feedStageReadContent  = "read_content"
	feedStageExtractURLs  = "extract_urls"
	feedStageRetirePage   = "retire_page"
	feedStageReleasePage  = "release_page"
	feedStageComplete     = "complete"
)

type feedProgressContextKey struct{}

type feedProgressTracker struct {
	mu           sync.Mutex
	lastProgress time.Time
	queryIndex   int
	queryTotal   int
	query        string
	stage        string
	iteration    int
	active       bool
}

type feedProgressSnapshot struct {
	LastProgress time.Time
	QueryIndex   int
	QueryTotal   int
	Query        string
	Stage        string
	Iteration    int
	Active       bool
}

func newFeedProgressTracker() *feedProgressTracker {
	return &feedProgressTracker{
		lastProgress: time.Now(),
		stage:        feedStageInitializing,
		active:       true,
	}
}

func withFeedProgress(ctx context.Context, tracker *feedProgressTracker) context.Context {
	return context.WithValue(ctx, feedProgressContextKey{}, tracker)
}

func feedProgressFromContext(ctx context.Context) *feedProgressTracker {
	tracker, _ := ctx.Value(feedProgressContextKey{}).(*feedProgressTracker)
	return tracker
}

func reportFeedProgress(ctx context.Context, stage string, iteration int) {
	if tracker := feedProgressFromContext(ctx); tracker != nil {
		tracker.advance(stage, iteration)
	}
}

func (t *feedProgressTracker) beginQuery(index, total int, query string) {
	t.mu.Lock()
	t.lastProgress = time.Now()
	t.queryIndex = index
	t.queryTotal = total
	t.query = query
	t.stage = feedStageAcquirePage
	t.iteration = 0
	t.active = true
	t.mu.Unlock()
}

func (t *feedProgressTracker) advance(stage string, iteration int) {
	t.mu.Lock()
	t.lastProgress = time.Now()
	t.stage = stage
	t.iteration = iteration
	t.mu.Unlock()
}

func (t *feedProgressTracker) complete() {
	t.mu.Lock()
	t.lastProgress = time.Now()
	t.stage = feedStageComplete
	t.iteration = 0
	t.active = false
	t.mu.Unlock()
}

func (t *feedProgressTracker) snapshot() feedProgressSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	return feedProgressSnapshot{
		LastProgress: t.lastProgress,
		QueryIndex:   t.queryIndex,
		QueryTotal:   t.queryTotal,
		Query:        t.query,
		Stage:        t.stage,
		Iteration:    t.iteration,
		Active:       t.active,
	}
}

// runFeedDiscoveryMonitor covers the part of Run before place workers and the
// regular worker stall monitor exist. A feed call can be blocked below rod's
// operation timeouts (for example in page/router teardown), so this watchdog
// exits the child process and lets the control process replace Chromium.
func (s *Scraper) runFeedDiscoveryMonitor(ctx context.Context, done <-chan struct{}, jobID string, tracker *feedProgressTracker) {
	hb := s.Config.HeartbeatInterval
	if hb <= 0 {
		hb = heartbeatInterval
	}
	stall := s.Config.StallTimeout
	if stall <= 0 {
		stall = stallTimeout
	}

	ticker := time.NewTicker(hb)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-ticker.C:
		}

		progress := tracker.snapshot()
		if !progress.Active {
			continue
		}
		idle := time.Since(progress.LastProgress)
		log.Printf("heartbeat: phase=feed_discovery query=%d/%d %q stage=%s iteration=%d idle=%s",
			progress.QueryIndex, progress.QueryTotal, progress.Query, progress.Stage, progress.Iteration, idle.Round(time.Second))
		if idle <= stall {
			continue
		}

		stallErr := fmt.Errorf("scraper: feed discovery stall: no progress for %s at %s (query %d/%d %q, iteration %d)",
			idle.Round(time.Second), progress.Stage, progress.QueryIndex, progress.QueryTotal, progress.Query, progress.Iteration)
		log.Printf("FEED STALL DETECTED: %v; dumping goroutine stacks", stallErr)
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		log.Printf("%s", buf[:n])

		// Mark inactive before invoking the injectable exit function. os.Exit
		// never returns in production; tests replace it with a recording stub.
		tracker.complete()
		if err := s.Store.SetJobStatus(context.Background(), jobID, JobStatusPaused, stallErr); err != nil {
			log.Printf("feed stall watchdog: set job %s paused: %v", jobID, err)
		}
		stallExit(ExitCodeStallWatchdog)
		return
	}
}
