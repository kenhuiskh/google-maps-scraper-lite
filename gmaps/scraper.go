package gmaps

import (
	"context"
	"errors"
	"log"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/playwright-community/playwright-go"
	"golang.org/x/sync/errgroup"
)

// Config controls what the Scraper extracts.
type Config struct {
	Concurrency      int
	MaxConcurrency   int
	ConcurrencyMode  string
	ConcurrencyValue int
	QueueWaitMinutes int
	Depth            int
	Lang             string
	Geo              string
	Radius           float64 // meters; 0 = no filter
	ExtractEmail     bool
	ExtraReviews     int
	Limit            int // max places to scrape; 0 = no limit
	JobID            string
	OutputMode       string // "database" or "file"; metadata for UI resume
	JSONOut          bool
	OutDir           string
	AutoRecover      bool
	RecoveryMinDelay time.Duration
	RecoveryMaxDelay time.Duration
	BrowseStartDelay time.Duration
}

// PagePool provides playwright pages to workers.
type PagePool interface {
	AcquirePage() playwright.Page
	ReleasePage(playwright.Page)
}

type PlaceResult struct {
	URLID int64
	URL   string
	Entry *Entry
}

// Scraper orchestrates the full scraping pipeline.
type Scraper struct {
	Config      Config
	Pool        PagePool
	Store       *JobStore
	ScrapePlace func(ctx context.Context, page playwright.Page, placeURL string, opts PlaceOptions) (*Entry, error)
	OnJobReady  func(jobID string)
}

// Run scrapes all queries and sends results to out. Run closes out when done.
// The caller must drain out concurrently or Run will deadlock.
func (s *Scraper) Run(ctx context.Context, queries []string, out chan<- PlaceResult) error {
	defer close(out)
	if s.Store == nil {
		return errors.New("scraper: job store is required")
	}
	scrapePlace := s.ScrapePlace
	if scrapePlace == nil {
		scrapePlace = ScrapePlace
	}

	feedOpts := FeedOptions{
		MaxDepth: s.Config.Depth,
		LangCode: s.Config.Lang,
		Geo:      s.Config.Geo,
	}
	placeOpts := PlaceOptions{
		ExtractEmail: s.Config.ExtractEmail,
		LangCode:     s.Config.Lang,
		ExtraReviews: s.Config.ExtraReviews,
	}

	jobID, err := s.ensureJob(ctx, queries, feedOpts)
	if err != nil {
		return err
	}

	conc := s.Config.Concurrency
	if conc < 1 {
		conc = 1
	}

	useRadiusFilter := s.Config.Radius > 0 && s.Config.Geo != ""
	var (
		mu      sync.Mutex
		entries []PlaceResult // only populated when useRadiusFilter
	)
	var completed int64
	var consecFails int64
	recovery := newRecoveryCoordinator(s.Store, jobID, s.Config)

	var g errgroup.Group
	gctx := ctx
	for i := 0; i < conc; i++ {
		workerID := i
		g.Go(func() error {
			rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(workerID)))
			if delay := s.Config.BrowseStartDelay; delay > 0 && workerID > 0 {
				if err := sleepContext(gctx, time.Duration(workerID)*delay); err != nil {
					return err
				}
			}
			for {
				if err := recovery.wait(gctx); err != nil {
					return err
				}

				claimed, err := s.Store.ClaimNextURL(gctx, jobID)
				if err != nil {
					switch {
					case errors.Is(err, ErrJobPaused):
						if s.Config.AutoRecover && recovery.isActive() {
							if waitErr := recovery.wait(gctx); waitErr != nil {
								return waitErr
							}
							continue
						}
						return ErrJobPaused
					case errors.Is(err, ErrNoPendingURL):
						return nil
					default:
						return err
					}
				}

				delay := s.Config.BrowseStartDelay
				if delay <= 0 {
					delay = time.Duration(rng.Intn(1000)) * time.Millisecond
				}
				if err := sleepContext(gctx, delay); err != nil {
					return err
				}

				page := s.Pool.AcquirePage()
				entry, err := scrapePlace(gctx, page, claimed.URL, placeOpts)
				for retry := 1; retry <= 2 && err != nil && strings.Contains(err.Error(), "Page crashed"); retry++ {
					s.Pool.ReleasePage(page)
					if err := sleepContext(gctx, time.Duration(retry*2)*time.Second); err != nil {
						return err
					}
					page = s.Pool.AcquirePage()
					entry, err = scrapePlace(gctx, page, claimed.URL, placeOpts)
				}
				s.Pool.ReleasePage(page)
				if err != nil {
					if s.Config.AutoRecover {
						_ = s.Store.RequeueURL(context.Background(), claimed.ID, err)
						log.Printf("place scrape error %s: %v", claimed.URL, err)
						if recoverErr := recovery.trigger(gctx, err, rng); recoverErr != nil {
							return recoverErr
						}
						continue
					}
					_ = s.Store.MarkURLFailed(context.Background(), claimed.ID, err)
					log.Printf("place scrape error %s: %v", claimed.URL, err)
					if atomic.AddInt64(&consecFails, 1) >= blockThreshold {
						log.Printf("session blocked after %d consecutive failures — job %s saved", blockThreshold, jobID)
						return ErrSessionBlocked
					}
					continue
				}

				atomic.StoreInt64(&consecFails, 0)
				done := atomic.AddInt64(&completed, 1)
				if done%10 == 0 {
					stats, _ := s.Store.JobStats(gctx, jobID)
					log.Printf("Places %d/%d completed", stats.Done, stats.Total)
				}

				result := PlaceResult{URLID: claimed.ID, URL: claimed.URL, Entry: entry}
				if useRadiusFilter {
					mu.Lock()
					entries = append(entries, result)
					mu.Unlock()
					if err := s.Store.MarkURLDone(gctx, claimed.ID); err != nil {
						return err
					}
				} else {
					select {
					case out <- result:
					case <-gctx.Done():
						return gctx.Err()
					}
				}
			}
		})
	}

	err = g.Wait()

	// Emit radius-filtered results even if the session was blocked mid-run.
	if useRadiusFilter && (err == nil || errors.Is(err, context.Canceled) || errors.Is(err, ErrSessionBlocked) || errors.Is(err, ErrJobPaused)) {
		lat, lon, _ := ParseGeoCenter(s.Config.Geo) // already validated by caller
		rawEntries := make([]*Entry, 0, len(entries))
		for _, result := range entries {
			rawEntries = append(rawEntries, result.Entry)
		}
		filtered := filterAndSortEntriesWithinRadius(rawEntries, lat, lon, s.Config.Radius)
		for _, e := range filtered {
			select {
			case out <- PlaceResult{Entry: e}:
			case <-ctx.Done():
				break
			}
		}
	}

	s.finishJob(jobID, err)
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

type recoveryCoordinator struct {
	store    *JobStore
	jobID    string
	enabled  bool
	minDelay time.Duration
	maxDelay time.Duration

	mu       sync.Mutex
	active   bool
	done     chan struct{}
	cooldown int
}

func newRecoveryCoordinator(store *JobStore, jobID string, cfg Config) *recoveryCoordinator {
	minDelay := cfg.RecoveryMinDelay
	if minDelay <= 0 {
		minDelay = 10 * time.Minute
	}
	maxDelay := cfg.RecoveryMaxDelay
	if maxDelay <= 0 {
		maxDelay = 60 * time.Minute
	}
	if maxDelay < minDelay {
		maxDelay = minDelay
	}
	return &recoveryCoordinator{
		store:    store,
		jobID:    jobID,
		enabled:  cfg.AutoRecover,
		minDelay: minDelay,
		maxDelay: maxDelay,
	}
}

func (r *recoveryCoordinator) wait(ctx context.Context) error {
	if !r.enabled {
		return nil
	}
	for {
		r.mu.Lock()
		done := r.done
		active := r.active
		r.mu.Unlock()
		if !active {
			return nil
		}
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (r *recoveryCoordinator) isActive() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active
}

func (r *recoveryCoordinator) trigger(ctx context.Context, scrapeErr error, rng *rand.Rand) error {
	if !r.enabled {
		return nil
	}
	r.mu.Lock()
	if r.active {
		done := r.done
		r.mu.Unlock()
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	r.active = true
	r.done = make(chan struct{})
	r.cooldown++
	done := r.done
	cooldown := r.cooldown
	delay := r.nextDelay(rng)
	r.mu.Unlock()

	if err := r.store.RequestPause(context.Background(), r.jobID); err != nil {
		r.finish()
		return err
	}
	_ = r.store.SetJobStatus(context.Background(), r.jobID, JobStatusPaused, scrapeErr)
	log.Printf("auto recovery pause %d for job %s: waiting %s before resume", cooldown, r.jobID, delay.Round(time.Second))

	err := sleepContext(ctx, delay)
	if err == nil {
		err = r.store.ResetInProgress(context.Background(), r.jobID)
	}
	if err == nil {
		err = r.store.ClearPause(context.Background(), r.jobID)
	}
	if err == nil {
		err = r.store.StartJob(context.Background(), r.jobID)
	}
	if err == nil {
		log.Printf("auto recovery resume %d for job %s", cooldown, r.jobID)
	}
	close(done)

	r.mu.Lock()
	if r.done == done {
		r.active = false
		r.done = nil
	}
	r.mu.Unlock()
	return err
}

func (r *recoveryCoordinator) nextDelay(rng *rand.Rand) time.Duration {
	if r.maxDelay <= r.minDelay {
		return r.minDelay
	}
	delta := r.maxDelay - r.minDelay
	return r.minDelay + time.Duration(rng.Int63n(int64(delta)+1))
}

func (r *recoveryCoordinator) finish() {
	r.mu.Lock()
	if r.active && r.done != nil {
		close(r.done)
	}
	r.active = false
	r.done = nil
	r.mu.Unlock()
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Scraper) ensureJob(ctx context.Context, queries []string, feedOpts FeedOptions) (string, error) {
	if s.Config.JobID != "" {
		job, err := s.Store.GetJob(ctx, s.Config.JobID)
		if err != nil {
			return "", err
		}
		if job.Status == JobStatusStarting {
			placeURLs := s.collectPlaceURLs(ctx, queries, feedOpts)
			if err := s.Store.QueueStartingJobURLs(ctx, s.Config.JobID, placeURLs); err != nil {
				return "", err
			}
			if err := s.Store.StartJob(ctx, s.Config.JobID); err != nil {
				return "", err
			}
			log.Printf("Started job %s: %d URLs queued", s.Config.JobID, len(placeURLs))
			if s.OnJobReady != nil {
				s.OnJobReady(s.Config.JobID)
			}
			return s.Config.JobID, nil
		}
		if err := s.Store.ResetInProgress(ctx, s.Config.JobID); err != nil {
			return "", err
		}
		if err := s.Store.StartJob(ctx, s.Config.JobID); err != nil {
			return "", err
		}
		stats, _ := s.Store.JobStats(ctx, s.Config.JobID)
		log.Printf("Resuming job %s: %d URLs total, %d done", s.Config.JobID, stats.Total, stats.Done)
		if s.OnJobReady != nil {
			s.OnJobReady(s.Config.JobID)
		}
		return s.Config.JobID, nil
	}

	placeURLs := s.collectPlaceURLs(ctx, queries, feedOpts)
	jobID, err := s.Store.CreateJob(ctx, queries, s.Config, placeURLs)
	if err != nil {
		return "", err
	}
	log.Printf("Created job %s", jobID)
	if err := s.Store.StartJob(ctx, jobID); err != nil {
		return "", err
	}
	s.Config.JobID = jobID
	if s.OnJobReady != nil {
		s.OnJobReady(jobID)
	}
	return jobID, nil
}

func (s *Scraper) collectPlaceURLs(ctx context.Context, queries []string, feedOpts FeedOptions) []string {
	var placeURLs []string
	total := len(queries)
	for i, q := range queries {
		log.Printf("Query %d/%d %q — starting", i+1, total, q)
		start := time.Now()
		page := s.Pool.AcquirePage()
		urls, err := ScrapeFeed(ctx, page, q, feedOpts)
		s.Pool.ReleasePage(page)
		if err != nil {
			log.Printf("Query %d/%d %q — feed error after %ds: %v", i+1, total, q, int(time.Since(start).Seconds()), err)
			continue
		}
		log.Printf("Query %d/%d %q — %d URLs found (%ds)", i+1, total, q, len(urls), int(time.Since(start).Seconds()))
		placeURLs = append(placeURLs, urls...)
	}

	originalCount := len(placeURLs)
	seen := make(map[string]struct{}, originalCount)
	dedupedURLs := make([]string, 0, originalCount)
	for _, u := range placeURLs {
		if _, ok := seen[u]; ok {
			continue
		}
		seen[u] = struct{}{}
		dedupedURLs = append(dedupedURLs, u)
	}
	placeURLs = dedupedURLs
	if s.Config.Limit > 0 && len(placeURLs) > s.Config.Limit {
		placeURLs = placeURLs[:s.Config.Limit]
	}
	duplicatesRemoved := originalCount - len(placeURLs)
	if duplicatesRemoved > 0 {
		log.Printf("Feed collection done: %d URLs queued across %d queries (%d duplicates removed)", len(placeURLs), total, duplicatesRemoved)
	} else {
		log.Printf("Feed collection done: %d URLs queued across %d queries", len(placeURLs), total)
	}
	return placeURLs
}

func (s *Scraper) finishJob(jobID string, err error) {
	switch {
	case err == nil:
		stats, statErr := s.Store.JobStats(context.Background(), jobID)
		if statErr == nil && stats.Pending == 0 && stats.InProgress == 0 {
			if stats.Failed > 0 {
				_ = s.Store.SetJobStatus(context.Background(), jobID, JobStatusFailed, nil)
				return
			}
			_ = s.Store.SetJobStatus(context.Background(), jobID, JobStatusDone, nil)
		}
	case errors.Is(err, ErrJobPaused):
		_ = s.Store.SetJobStatus(context.Background(), jobID, JobStatusPaused, nil)
	case errors.Is(err, ErrSessionBlocked):
		_ = s.Store.SetJobStatus(context.Background(), jobID, JobStatusBlocked, err)
	case !errors.Is(err, context.Canceled):
		_ = s.Store.SetJobStatus(context.Background(), jobID, JobStatusFailed, err)
	}
}
