package gmaps

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
)

// ErrScrapeDeadline is returned when a single place scrape exceeds the
// per-scrape watchdog deadline. The page has been closed to unblock the wedged
// underlying page-driver call, so the same page object can never be retried.
var ErrScrapeDeadline = errors.New("scraper: scrape watchdog deadline exceeded")

// scrapeDeadline is the default hard cap on a single place scrape.
// Page-driver calls do not honor Go contexts, so without it a wedged call
// blocks a worker forever.
const scrapeDeadline = 4 * time.Minute

// heartbeatInterval and stallTimeout are the stall-monitor defaults applied
// when the corresponding Config fields are zero.
const (
	heartbeatInterval = 5 * time.Minute
	stallTimeout      = 15 * time.Minute
)

// ExitCodeStallWatchdog is the exit code used when the stall monitor detects
// no pipeline progress for StallTimeout. The job has already been marked
// paused with its in-progress URLs reset, so control.go's spawnProcess
// auto-resumes the subprocess (control.go keeps its own local
// exitCodeStallWatchdog constant; the two values must stay in sync).
const ExitCodeStallWatchdog = 3

// stallExit is os.Exit, swappable so tests can intercept the watchdog exit.
var stallExit = os.Exit

// isTransientNavError reports whether err is a transient navigation/page error
// that warrants a fast in-process retry rather than the long recovery pause.
// ErrScrapeDeadline is excluded: the watchdog closed the page, so the claim
// must go back through the requeue path instead of a same-page retry.
func isTransientNavError(err error) bool {
	if err == nil || errors.Is(err, ErrScrapeDeadline) {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Timeout") ||
		strings.Contains(msg, "net::ERR") ||
		strings.Contains(msg, "Page crashed")
}

// Config controls what the Scraper extracts.
type Config struct {
	Concurrency       int
	MaxConcurrency    int
	ConcurrencyMode   string
	ConcurrencyValue  int
	QueueWaitMinutes  int
	Depth             int
	Lang              string
	Geo               string
	Radius            float64       // meters; 0 = no filter
	HeartbeatInterval time.Duration // monitor heartbeat log period; 0 = heartbeatInterval default
	StallTimeout      time.Duration // no-progress watchdog exit threshold; 0 = stallTimeout default
	ExtractEmail      bool
	ExtraReviews      int
	EnableHTTPFirst   bool // true = opt in to the HTTP-first place path; default is the browser
	Limit             int  // max places to scrape; 0 = no limit
	JobID             string
	OutputMode        string // "database" or "file"; metadata for UI resume
	JSONOut           bool
	OutDir            string
	AutoRecover       bool
	RecoveryMinDelay  time.Duration
	RecoveryMaxDelay  time.Duration
	BrowseStartDelay  time.Duration
	DedupScope        string        // "" (off) | "run" (same strategy run) | "all" (any prior job)
	ScrapeDeadline    time.Duration // per-scrape watchdog; 0 = scrapeDeadline default
	MaxURLAttempts    int           // DB claims per queued URL before final failure; 0 = no cap
}

// PagePool provides browser pages to workers.
type PagePool interface {
	AcquirePage(ctx context.Context) (Page, error)
	ReleasePage(Page)
}

type PlaceResult struct {
	URLID int64
	URL   string
	Lang  string
	Entry *Entry
}

// normalizeLangs parses a comma-separated language string into a normalized,
// de-duplicated list (trimmed, lowercased, empties dropped). It always returns
// at least one language, defaulting to "en". Collision validation (codes that
// sanitize to the same output table/file suffix) is the caller's responsibility
// at the CLI boundary.
func normalizeLangs(s string) []string {
	seen := make(map[string]struct{})
	var langs []string
	for _, part := range strings.Split(s, ",") {
		lang := strings.ToLower(strings.TrimSpace(part))
		if lang == "" {
			continue
		}
		if _, ok := seen[lang]; ok {
			continue
		}
		seen[lang] = struct{}{}
		langs = append(langs, lang)
	}
	if len(langs) == 0 {
		return []string{"en"}
	}
	return langs
}

// expandLangs fans out each canonical place URL into one QueuedURL per language.
// Ordering is URL-major (all languages of a place are adjacent) so both
// languages progress together under concurrent claims.
func expandLangs(urls []string, langs []string) []QueuedURL {
	out := make([]QueuedURL, 0, len(urls)*len(langs))
	for _, u := range urls {
		for _, lang := range langs {
			out = append(out, QueuedURL{URL: u, Lang: lang})
		}
	}
	return out
}

// Scraper orchestrates the full scraping pipeline.
type Scraper struct {
	Config      Config
	Pool        PagePool
	Store       *JobStore
	ScrapePlace func(ctx context.Context, page Page, placeURL string, opts PlaceOptions) (*Entry, error)
	// ScrapePlaceHTTP fetches place details over HTTP without a browser page.
	// It is tried first for each place when Config.EnableHTTPFirst is set (and
	// the claim does not need ExtraReviews); ScrapePlace is the fallback.
	ScrapePlaceHTTP func(ctx context.Context, placeURL string, opts PlaceOptions) (*Entry, error)
	ScrapeFeed      func(ctx context.Context, page Page, query string, opts FeedOptions) ([]string, error)
	OnJobReady      func(jobID string)
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
	scrapePlaceHTTP := s.ScrapePlaceHTTP
	if scrapePlaceHTTP == nil {
		scrapePlaceHTTP = ScrapePlaceHTTP
	}

	langs := normalizeLangs(s.Config.Lang)
	feedOpts := FeedOptions{
		MaxDepth: s.Config.Depth,
		LangCode: langs[0],
		Geo:      s.Config.Geo,
	}
	// placeOpts.LangCode is set per claimed URL from the row's language.
	placeOpts := PlaceOptions{
		ExtractEmail: s.Config.ExtractEmail,
		ExtraReviews: s.Config.ExtraReviews,
	}

	jobID, err := s.ensureJob(ctx, queries, feedOpts, langs)
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

	// Stall-monitor state: lastProgress is bumped on every successful claim
	// and every finished scrape (success or handled failure); inflight names
	// what each worker currently holds for heartbeat/stall diagnostics.
	var lastProgress atomic.Int64
	lastProgress.Store(time.Now().UnixNano())
	inflight := newInflightRegistry()

	g, gctx := errgroup.WithContext(ctx)
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

				inflight.set(workerID, claimed.URL)
				lastProgress.Store(time.Now().UnixNano())

				delay := s.Config.BrowseStartDelay
				if delay <= 0 {
					delay = time.Duration(rng.Intn(1000)) * time.Millisecond
				}
				if err := sleepContext(gctx, delay); err != nil {
					return err
				}

				claimOpts := placeOpts
				claimOpts.LangCode = claimed.Lang
				if claimOpts.LangCode == "" {
					// Legacy rows predate the lang column; fall back to the
					// job's configured first language.
					claimOpts.LangCode = langs[0]
				}

				var entry *Entry
				usedHTTP := false
				if s.Config.EnableHTTPFirst && claimOpts.ExtraReviews == 0 {
					entry, err = scrapePlaceHTTP(gctx, claimed.URL, claimOpts)
					if err == nil {
						usedHTTP = true
					}
					// Any HTTP error (unavailable, bot-block, or otherwise) is not
					// surfaced here — it always degrades to the browser path below,
					// it never fails the URL by itself.
				}
				if !usedHTTP {
					// An acquire failure means the browser is dead: end the run so
					// finishJob persists the job state instead of hanging workers.
					page, aerr := s.Pool.AcquirePage(gctx)
					if aerr != nil {
						return aerr
					}
					entry, err = s.scrapeWithDeadline(gctx, scrapePlace, page, claimed.URL, claimOpts)
					for retry := 1; retry <= 2 && err != nil && isTransientNavError(err) && !errors.Is(err, ErrBotBlocked); retry++ {
						s.releaseScrapePage(page, err)
						if serr := sleepContext(gctx, time.Duration(retry*2)*time.Second); serr != nil {
							return serr
						}
						page, aerr = s.Pool.AcquirePage(gctx)
						if aerr != nil {
							return aerr
						}
						entry, err = s.scrapeWithDeadline(gctx, scrapePlace, page, claimed.URL, claimOpts)
					}
					s.releaseScrapePage(page, err)
				}
				if err == nil {
					// Lightweight operator visibility into the HTTP-first split.
					// IncrementJobStat's field whitelist doesn't cover
					// http_scrapes/browser_scrapes, so this is a log line rather
					// than a DB counter (see task-2 brief §4).
					if usedHTTP {
						log.Printf("scrape via http: %s", claimed.URL)
					} else {
						log.Printf("scrape via browser: %s", claimed.URL)
					}
				}
				if err != nil {
					// A handled failure still counts as pipeline progress.
					inflight.clear(workerID)
					lastProgress.Store(time.Now().UnixNano())
					if s.Config.AutoRecover {
						if s.urlAttemptsExhausted(claimed) {
							_ = s.Store.IncrementJobStat(context.Background(), jobID, "scrape_errors", 1)
							_ = s.Store.MarkURLFailed(context.Background(), claimed.ID, err)
							log.Printf("place scrape error %s: %v — attempts exhausted (%d/%d)", claimed.URL, err, claimed.Attempts, s.Config.MaxURLAttempts)
							continue
						}
						_ = s.Store.RequeueURL(context.Background(), claimed.ID, err)
						log.Printf("place scrape error %s: %v", claimed.URL, err)
						if errors.Is(err, ErrBotBlocked) {
							log.Printf("bot block detected — auto recovery for job %s", jobID)
							if recoverErr := recovery.trigger(gctx, err, rng); recoverErr != nil {
								return recoverErr
							}
							continue
						}
						if atomic.AddInt64(&consecFails, 1) >= blockThreshold {
							log.Printf("session blocked after %d consecutive failures — auto recovery for job %s", blockThreshold, jobID)
							if recoverErr := recovery.trigger(gctx, err, rng); recoverErr != nil {
								return recoverErr
							}
						}
						continue
					}
					_ = s.Store.IncrementJobStat(context.Background(), jobID, "scrape_errors", 1)
					_ = s.Store.MarkURLFailed(context.Background(), claimed.ID, err)
					log.Printf("place scrape error %s: %v", claimed.URL, err)
					if atomic.AddInt64(&consecFails, 1) >= blockThreshold {
						log.Printf("session blocked after %d consecutive failures — job %s saved", blockThreshold, jobID)
						return ErrSessionBlocked
					}
					continue
				}

				atomic.StoreInt64(&consecFails, 0)
				inflight.clear(workerID)
				lastProgress.Store(time.Now().UnixNano())
				done := atomic.AddInt64(&completed, 1)
				if done%10 == 0 {
					stats, _ := s.Store.JobStats(gctx, jobID)
					log.Printf("Places %d/%d completed", stats.Done, stats.Total)
				}

				result := PlaceResult{URLID: claimed.ID, URL: claimed.URL, Lang: claimOpts.LangCode, Entry: entry}
				if useRadiusFilter {
					// Defer MarkURLDone until after the radius filter: filtered-out
					// entries are marked done here (no write needed); kept entries
					// keep their URLID so the writer can mark done after write
					// succeeds or failed if the write errors.
					mu.Lock()
					entries = append(entries, result)
					mu.Unlock()
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

	// The monitor lives only while workers run; Run blocks on monitorStopped
	// so a normal completion never leaks the goroutine.
	monitorDone := make(chan struct{})
	monitorStopped := make(chan struct{})
	go func() {
		defer close(monitorStopped)
		s.runStallMonitor(ctx, monitorDone, jobID, recovery, inflight, &lastProgress)
	}()

	err = g.Wait()
	close(monitorDone)
	<-monitorStopped

	// Emit radius-filtered results even if the session was blocked mid-run.
	if useRadiusFilter && (err == nil || errors.Is(err, context.Canceled) || errors.Is(err, ErrSessionBlocked) || errors.Is(err, ErrJobPaused)) {
		lat, lon, _ := ParseGeoCenter(s.Config.Geo) // already validated by caller
		rawEntries := make([]*Entry, 0, len(entries))
		entryURLID := make(map[*Entry]int64, len(entries))
		entryURL := make(map[*Entry]string, len(entries))
		entryLang := make(map[*Entry]string, len(entries))
		for _, result := range entries {
			rawEntries = append(rawEntries, result.Entry)
			entryURLID[result.Entry] = result.URLID
			entryURL[result.Entry] = result.URL
			entryLang[result.Entry] = result.Lang
		}
		filtered := filterAndSortEntriesWithinRadius(rawEntries, lat, lon, s.Config.Radius)
		keep := make(map[*Entry]struct{}, len(filtered))
		for _, e := range filtered {
			keep[e] = struct{}{}
		}
		// Mark filtered-out URLs done now; writer will never see them.
		for _, result := range entries {
			if _, ok := keep[result.Entry]; ok {
				continue
			}
			if result.URLID != 0 {
				_ = s.Store.MarkURLDone(context.Background(), result.URLID)
			}
		}
		for _, e := range filtered {
			select {
			case out <- PlaceResult{URLID: entryURLID[e], URL: entryURL[e], Lang: entryLang[e], Entry: e}:
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

// scrapeWithDeadline runs one place scrape under a hard watchdog. Page-driver
// calls do not honor ctx, so on deadline (or cancellation) the page is closed to
// force the wedged call inside the scrape to error out. The result channel is
// buffered so the scrape goroutine can always deliver and never leaks.
func (s *Scraper) scrapeWithDeadline(ctx context.Context, scrape func(ctx context.Context, page Page, placeURL string, opts PlaceOptions) (*Entry, error), page Page, placeURL string, opts PlaceOptions) (*Entry, error) {
	deadline := s.Config.ScrapeDeadline
	if deadline <= 0 {
		deadline = scrapeDeadline
	}
	type scrapeResult struct {
		entry *Entry
		err   error
	}
	res := make(chan scrapeResult, 1)
	go func() {
		entry, err := scrape(ctx, page, placeURL, opts)
		res <- scrapeResult{entry, err}
	}()

	timer := time.NewTimer(deadline)
	defer timer.Stop()
	select {
	case r := <-res:
		return r.entry, r.err
	case <-ctx.Done():
		if page != nil {
			_ = page.Close()
		}
		return nil, ctx.Err()
	case <-timer.C:
		if page != nil {
			_ = page.Close()
		}
		log.Printf("scrape watchdog: %s exceeded %s — page closed", placeURL, deadline)
		return nil, ErrScrapeDeadline
	}
}

// runStallMonitor logs a periodic heartbeat and, when the pipeline makes no
// progress for StallTimeout while work is still claimed or queued, dumps all
// goroutine stacks, marks the job paused with its in-progress URLs reset, and
// exits the process with ExitCodeStallWatchdog so control.go auto-resumes it.
// Stall detection is skipped while a recovery pause is active: a 10-60min
// recovery sleep is intentional no-progress.
func (s *Scraper) runStallMonitor(ctx context.Context, done <-chan struct{}, jobID string, recovery *recoveryCoordinator, inflight *inflightRegistry, lastProgress *atomic.Int64) {
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
		case <-done:
			return
		case <-ticker.C:
		}

		// One JobStats query per tick serves both the heartbeat line and the
		// pending-work half of the stall condition.
		stats, statsErr := s.Store.JobStats(ctx, jobID)
		if statsErr != nil {
			log.Printf("heartbeat: stats unavailable (%v) inflight=%s", statsErr, inflight.snapshot())
		} else {
			log.Printf("heartbeat: done=%d/%d inflight=%s", stats.Done, stats.Total, inflight.snapshot())
		}

		idle := time.Since(time.Unix(0, lastProgress.Load()))
		if idle <= stall || recovery.isActive() {
			continue
		}
		hasWork := inflight.size() > 0 || (statsErr == nil && (stats.Pending > 0 || stats.InProgress > 0))
		if !hasWork {
			continue
		}

		log.Printf("STALL DETECTED: no progress for %s; dumping goroutine stacks; inflight=%s", idle.Round(time.Second), inflight.snapshot())
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		log.Printf("%s", buf[:n])

		stallErr := fmt.Errorf("scraper: stall watchdog: no progress for %s", idle.Round(time.Second))
		if err := s.Store.ResetInProgress(context.Background(), jobID); err != nil {
			log.Printf("stall watchdog: reset in-progress URLs for job %s: %v", jobID, err)
		}
		if err := s.Store.SetJobStatus(context.Background(), jobID, JobStatusPaused, stallErr); err != nil {
			log.Printf("stall watchdog: set job %s paused: %v", jobID, err)
		}
		stallExit(ExitCodeStallWatchdog)
		return // reached only when tests swap stallExit
	}
}

// inflightRegistry tracks the URL each worker currently holds so heartbeat and
// stall diagnostics can name the wedged claims.
type inflightRegistry struct {
	mu      sync.Mutex
	entries map[int]inflightClaim
}

type inflightClaim struct {
	url     string
	started time.Time
}

func newInflightRegistry() *inflightRegistry {
	return &inflightRegistry{entries: make(map[int]inflightClaim)}
}

func (r *inflightRegistry) set(worker int, url string) {
	r.mu.Lock()
	r.entries[worker] = inflightClaim{url: url, started: time.Now()}
	r.mu.Unlock()
}

func (r *inflightRegistry) clear(worker int) {
	r.mu.Lock()
	delete(r.entries, worker)
	r.mu.Unlock()
}

func (r *inflightRegistry) size() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

// snapshot renders "[0: <url> 35s, 1: <url> 12s]" ordered by worker id.
func (r *inflightRegistry) snapshot() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]int, 0, len(r.entries))
	for id := range r.entries {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	var b strings.Builder
	b.WriteByte('[')
	for i, id := range ids {
		if i > 0 {
			b.WriteString(", ")
		}
		claim := r.entries[id]
		fmt.Fprintf(&b, "%d: %s %s", id, claim.url, time.Since(claim.started).Round(time.Second))
	}
	b.WriteByte(']')
	return b.String()
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

func (s *Scraper) ensureJob(ctx context.Context, queries []string, feedOpts FeedOptions, langs []string) (string, error) {
	if s.Config.JobID != "" {
		job, err := s.Store.GetJob(ctx, s.Config.JobID)
		if err != nil {
			return "", err
		}
		if job.Status == JobStatusStarting {
			tracker := newFeedProgressTracker()
			monitorDone := make(chan struct{})
			monitorStopped := make(chan struct{})
			go func() {
				defer close(monitorStopped)
				s.runFeedDiscoveryMonitor(ctx, monitorDone, s.Config.JobID, tracker)
			}()

			collected, err := s.collectPlaceURLs(withFeedProgress(ctx, tracker), queries, feedOpts, job.StrategyRunID.String, langs)
			tracker.complete()
			close(monitorDone)
			<-monitorStopped
			if err != nil {
				return "", err
			}
			if err := s.Store.QueueStartingJobURLs(ctx, s.Config.JobID, collected.URLs); err != nil {
				return "", err
			}
			_ = s.Store.SetJobDiscoveryStats(ctx, s.Config.JobID, collected.FeedURLsFound, collected.FeedDuplicateURLs, collected.CrossJobDuplicateURLs, len(collected.URLs))
			if err := s.Store.StartJob(ctx, s.Config.JobID); err != nil {
				return "", err
			}
			log.Printf("Started job %s: %d URLs queued", s.Config.JobID, len(collected.URLs))
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

	collected, err := s.collectPlaceURLs(ctx, queries, feedOpts, "", langs)
	if err != nil {
		return "", err
	}
	jobID, err := s.Store.CreateJob(ctx, queries, s.Config, collected.URLs)
	if err != nil {
		return "", err
	}
	_ = s.Store.SetJobDiscoveryStats(ctx, jobID, collected.FeedURLsFound, collected.FeedDuplicateURLs, collected.CrossJobDuplicateURLs, len(collected.URLs))
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

type feedCollection struct {
	URLs                  []QueuedURL
	FeedURLsFound         int
	FeedDuplicateURLs     int
	CrossJobDuplicateURLs int
}

func (s *Scraper) collectPlaceURLs(ctx context.Context, queries []string, feedOpts FeedOptions, strategyRunID string, langs []string) (feedCollection, error) {
	scrapeFeed := s.ScrapeFeed
	if scrapeFeed == nil {
		scrapeFeed = ScrapeFeed
	}

	var feedURLs []string
	total := len(queries)
	tracker := feedProgressFromContext(ctx)
	if tracker != nil {
		defer tracker.complete()
	}
	for i, q := range queries {
		if tracker != nil {
			tracker.beginQuery(i+1, total, q)
		}
		log.Printf("Query %d/%d %q — starting", i+1, total, q)
		start := time.Now()
		reportFeedProgress(ctx, feedStageAcquirePage, 0)
		page, err := s.Pool.AcquirePage(ctx)
		if err != nil {
			// The feed phase cannot proceed without a page; a dead browser
			// must abort the run rather than skip queries silently.
			log.Printf("Query %d/%d %q — acquire page: %v", i+1, total, q, err)
			return feedCollection{}, err
		}
		reportFeedProgress(ctx, feedStageScrapeFeed, 0)
		urls, err := scrapeFeed(ctx, page, q, feedOpts)
		if err != nil {
			reportFeedProgress(ctx, feedStageRetirePage, 0)
		} else {
			reportFeedProgress(ctx, feedStageReleasePage, 0)
		}
		s.releaseFeedPage(page, err)
		if err != nil {
			if id, ok := strings.CutPrefix(q, "place_id:"); ok {
				fallbackURL := PlaceIDToURL(id)
				log.Printf("Query %d/%d %q — feed error after %ds, using direct place-ID URL: %v", i+1, total, q, int(time.Since(start).Seconds()), err)
				feedURLs = append(feedURLs, fallbackURL)
				continue
			}
			log.Printf("Query %d/%d %q — feed error after %ds: %v", i+1, total, q, int(time.Since(start).Seconds()), err)
			continue
		}
		if len(urls) == 0 {
			if id, ok := strings.CutPrefix(q, "place_id:"); ok {
				fallbackURL := PlaceIDToURL(id)
				log.Printf("Query %d/%d %q — no feed URLs found, using direct place-ID URL", i+1, total, q)
				feedURLs = append(feedURLs, fallbackURL)
				continue
			}
		}
		log.Printf("Query %d/%d %q — %d URLs found (%ds)", i+1, total, q, len(urls), int(time.Since(start).Seconds()))
		feedURLs = append(feedURLs, urls...)
		reportFeedProgress(ctx, feedStageComplete, 0)
	}

	feedURLsFound := len(feedURLs)
	originalCount := len(feedURLs)
	seen := make(map[string]struct{}, originalCount)
	dedupedURLs := make([]string, 0, originalCount)
	for _, u := range feedURLs {
		if _, ok := seen[u]; ok {
			continue
		}
		seen[u] = struct{}{}
		dedupedURLs = append(dedupedURLs, u)
	}
	feedURLs = dedupedURLs
	duplicatesRemoved := originalCount - len(feedURLs)
	// Limit caps canonical places, not place×language tasks, so apply it before
	// fanning out into per-language queued URLs.
	if s.Config.Limit > 0 && len(feedURLs) > s.Config.Limit {
		feedURLs = feedURLs[:s.Config.Limit]
	}
	queued := expandLangs(feedURLs, langs)
	crossJobDuplicates := 0
	dedupScraped := s.Config.DedupScope == "run" || s.Config.DedupScope == "all"
	if dedupScraped {
		kept, skipped, err := s.Store.FilterAlreadyScrapedURLs(ctx, queued, strategyRunID, s.Config.DedupScope == "all", langs[0])
		if err != nil {
			return feedCollection{}, err
		}
		queued = kept
		crossJobDuplicates = skipped
	}
	if dedupScraped {
		log.Printf("Feed collection done: %d URL×lang tasks queued across %d queries, %d languages (%d duplicates removed, %d already-scraped skipped)", len(queued), total, len(langs), duplicatesRemoved, crossJobDuplicates)
	} else if duplicatesRemoved > 0 {
		log.Printf("Feed collection done: %d URL×lang tasks queued across %d queries, %d languages (%d duplicates removed)", len(queued), total, len(langs), duplicatesRemoved)
	} else {
		log.Printf("Feed collection done: %d URL×lang tasks queued across %d queries, %d languages", len(queued), total, len(langs))
	}
	return feedCollection{URLs: queued, FeedURLsFound: feedURLsFound, FeedDuplicateURLs: duplicatesRemoved, CrossJobDuplicateURLs: crossJobDuplicates}, nil
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
