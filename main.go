package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gosom/google-maps-scraper-lite/browser"
	"github.com/gosom/google-maps-scraper-lite/gmaps"
	"github.com/gosom/google-maps-scraper-lite/output"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "suggest-zoom" {
		runSuggestZoom(os.Args[2:])
		return
	}

	queries := flag.String("queries", "", "comma-separated search queries (mutually exclusive with -place-ids)")
	placeIDs := flag.String("place-ids", "", "comma-separated Google Maps place IDs, e.g. ChIJN1t_tDeuEmsRUsoyG83frY4 (mutually exclusive with -queries; max 2000)")
	jobID := flag.String("job", "", "SQLite job ID to resume")
	stateDB := flag.String("state-db", filepath.Join("gmdata", "scraper-state.sqlite"), "path to local SQLite scraper state database")
	controlAddr := flag.String("control-addr", "", "optional local HTTP control UI address, e.g. 127.0.0.1:8080")
	concurrency := flag.Int("c", 1, "concurrency level")
	maxConcurrency := flag.Int("max-c", 0, "maximum dynamic browser tab concurrency; overrides -c when set")
	depth := flag.Int("depth", 10, "max scroll depth per query")
	jsonOut := flag.Bool("json", false, "output as JSON instead of CSV")
	outDir := flag.String("o", "gmdata", "output directory for CSV/JSON files (ignored when -dsn is set)")
	dsn := flag.String("dsn", "", "postgres connection string (e.g. postgres://user:pass@host/db)")
	tableRestaurant := flag.String("table-restaurant", "restaurants", "postgres table name for places (used with -dsn)")
	tableReview := flag.String("table-review", "restaurant_reviews", "postgres table name for reviews (used with -dsn)")
	extractEmail := flag.Bool("email", false, "extract emails from websites")
	extraReviews := flag.Int("reviews", 0, "minimum number of reviews to scrape (0 = use page default)")
	lang := flag.String("lang", "en", "language code")
	geo := flag.String("geo", "", `geographic center for search, format "lat,lng,zoomz" e.g. "43.6532,-79.3832,14z"`)
	radius := flag.Float64("radius", 0, "filter results within this radius in meters from -geo center (0 = no filter)")
	headless := flag.Bool("headless", true, "run browser in headless mode")
	errorLog := flag.String("error-log", "", "path to error log file (appended; default: stderr only)")
	urlsOnly := flag.String("urls-only", "", "debug: collect feed URLs only and write to this file (no place scraping)")
	limit := flag.Int("limit", 0, "max number of places to scrape (0 = no limit)")
	flag.Parse()

	explicitTables := map[string]bool{}
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "table-restaurant" || f.Name == "table-review" {
			explicitTables[f.Name] = true
		}
	})

	if *maxConcurrency > 0 {
		*concurrency = *maxConcurrency
	}
	if *concurrency < 1 {
		*concurrency = 1
	}

	if *errorLog != "" {
		f, err := os.OpenFile(*errorLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			log.Fatalf("open error log: %v", err)
		}
		defer f.Close()
		log.SetOutput(io.MultiWriter(os.Stderr, f))
	}

	// Allow the DSN to be supplied via environment so that subprocesses
	// spawned by the control UI do not need it as a visible CLI argument.
	if *dsn == "" {
		*dsn = os.Getenv("DSN")
	}

	if *queries != "" && *placeIDs != "" {
		fmt.Fprintln(os.Stderr, "error: -queries and -place-ids are mutually exclusive")
		flag.Usage()
		os.Exit(1)
	}

	if *jobID == "" && *queries == "" && *placeIDs == "" && *controlAddr == "" {
		fmt.Fprintln(os.Stderr, "error: -queries or -place-ids is required (or use -job to continue a previous run, or -control-addr for UI-only mode)")
		flag.Usage()
		os.Exit(1)
	}

	if *radius > 0 && *geo == "" {
		fmt.Fprintln(os.Stderr, "error: -radius requires -geo to be set")
		flag.Usage()
		os.Exit(1)
	}

	if *geo != "" {
		if _, _, err := gmaps.ParseGeoCenter(*geo); err != nil {
			fmt.Fprintf(os.Stderr, "error: invalid -geo value: %v\n", err)
			flag.Usage()
			os.Exit(1)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if *urlsOnly != "" {
		runURLsOnly(ctx, *urlsOnly, *queries, *depth, *lang, *geo)
		return
	}

	store, err := gmaps.OpenJobStore(*stateDB)
	if err != nil {
		log.Fatalf("open state db: %v", err)
	}
	defer store.Close()
	log.Printf("using state db %s", *stateDB)

	if *controlAddr != "" {
		if _, err := startControlServer(ctx, *controlAddr, store, *stateDB, newProcessResumeLauncher(store, *stateDB), newProcessStartLauncher(*stateDB)); err != nil {
			log.Fatalf("control server: %v", err)
		}
	}

	if *jobID == "" && *queries == "" {
		log.Printf("control UI only; press Ctrl-C to stop")
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		defer signal.Stop(sigCh)
		select {
		case sig := <-sigCh:
			log.Printf("received %s: shutting down control UI", sig)
		case <-ctx.Done():
		}
		return
	}

	var (
		currentJobMu sync.Mutex
		currentJobID = *jobID
		pauseWanted  bool
		forceStop    bool
	)
	requestPause := func() {
		currentJobMu.Lock()
		defer currentJobMu.Unlock()
		pauseWanted = true
		if currentJobID == "" {
			log.Printf("pause requested; it will apply after the job is created")
			return
		}
		if err := store.RequestPause(context.Background(), currentJobID); err != nil {
			log.Printf("pause request error: %v", err)
			return
		}
		log.Printf("pause requested for job %s; active URL scrapes will finish", currentJobID)
	}
	setCurrentJob := func(id string) {
		currentJobMu.Lock()
		defer currentJobMu.Unlock()
		currentJobID = id
		if pauseWanted {
			if err := store.RequestPause(context.Background(), currentJobID); err != nil {
				log.Printf("pause request error: %v", err)
			}
		}
	}

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		for sig := range sigCh {
			currentJobMu.Lock()
			alreadyPauseWanted := pauseWanted
			currentJobMu.Unlock()
			if !alreadyPauseWanted {
				log.Printf("received %s: requesting graceful pause", sig)
				requestPause()
				continue
			}
			currentJobMu.Lock()
			if !forceStop {
				forceStop = true
				currentJobMu.Unlock()
				log.Printf("received %s again: forcing shutdown", sig)
				cancel()
				continue
			}
			currentJobMu.Unlock()
		}
	}()

	br, err := browser.New(browser.Options{
		Concurrency: *concurrency,
		Headless:    *headless,
		Lang:        *lang,
	})
	if err != nil {
		log.Fatalf("browser init: %v", err)
	}
	defer br.Close()

	ts := time.Now().Format("2006-01-02_15-04-05")

	// Ensure output dir exists (needed for file output and the default state DB).
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		log.Fatalf("create output dir: %v", err)
	}

	var w output.Writer
	switch {
	case *dsn != "":
		defaultRestaurant, defaultReview := languagePostgresTables(*lang)
		if !explicitTables["table-restaurant"] {
			*tableRestaurant = defaultRestaurant
		}
		if !explicitTables["table-review"] {
			*tableReview = defaultReview
		}
		pw, err := output.NewPostgresWriter(ctx, *dsn, *tableRestaurant, *tableReview)
		if err != nil {
			log.Fatalf("postgres writer init: %v", err)
		}
		w = pw
	default:
		ext := ".csv"
		if *jsonOut {
			ext = ".json"
		}
		outPath := filepath.Join(*outDir, ts+ext)
		f, err := os.Create(outPath)
		if err != nil {
			log.Fatalf("create output file: %v", err)
		}
		defer f.Close()
		log.Printf("writing output to %s", outPath)
		if *jsonOut {
			w = output.NewJSONWriter(f)
		} else {
			w = output.NewCSVWriter(f)
		}
	}

	out := make(chan gmaps.PlaceResult, *concurrency*2)

	s := gmaps.Scraper{
		Config: gmaps.Config{
			Concurrency:      *concurrency,
			MaxConcurrency:   *maxConcurrency,
			Depth:            *depth,
			Lang:             *lang,
			Geo:              *geo,
			Radius:           *radius,
			ExtractEmail:     *extractEmail,
			ExtraReviews:     *extraReviews,
			Limit:            *limit,
			JobID:            *jobID,
			OutputMode:       outputMode(*dsn),
			JSONOut:          *jsonOut,
			OutDir:           *outDir,
			AutoRecover:      true,
			RecoveryMinDelay: 10 * time.Minute,
			RecoveryMaxDelay: 60 * time.Minute,
			BrowseStartDelay: 500 * time.Millisecond,
		},
		Pool:       br,
		Store:      store,
		OnJobReady: setCurrentJob,
	}

	var qs []string
	if *jobID == "" {
		if *placeIDs != "" {
			ids := strings.Split(*placeIDs, ",")
			for _, id := range ids {
				id = strings.TrimSpace(id)
				if id == "" {
					continue
				}
				qs = append(qs, "place_id:"+id)
			}
			if len(qs) > 2000 {
				fmt.Fprintf(os.Stderr, "error: -place-ids accepts at most 2000 IDs, got %d\n", len(qs))
				os.Exit(1)
			}
		} else {
			parts := strings.Split(*queries, ",")
			for _, q := range parts {
				q = strings.TrimSpace(q)
				if q != "" {
					qs = append(qs, q)
				}
			}
		}
	} else {
		job, err := store.GetJob(ctx, *jobID)
		if err != nil {
			log.Fatalf("load job: %v", err)
		}
		qs = job.Queries
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.Run(ctx, qs, out)
	}()

	var written, failed, dupes int
	dedupe := newPlaceDeduper()
	for result := range out {
		currentJobMu.Lock()
		statsJobID := currentJobID
		currentJobMu.Unlock()
		if statsJobID != "" {
			_ = store.IncrementJobStat(context.Background(), statsJobID, "scraped_urls", 1)
		}
		entry := result.Entry
		if dedupe.Seen(entry) {
			dupes++
			if statsJobID != "" {
				_ = store.IncrementJobStat(context.Background(), statsJobID, "duplicate_places", 1)
			}
			if result.URLID != 0 {
				_ = store.MarkURLDone(context.Background(), result.URLID)
			}
			continue
		}
		if err := w.Write(entry); err != nil {
			log.Printf("write error: %v", err)
			if result.URLID != 0 {
				_ = store.MarkURLFailed(context.Background(), result.URLID, err)
			}
			if statsJobID != "" {
				_ = store.IncrementJobStat(context.Background(), statsJobID, "write_errors", 1)
			}
			failed++
		} else {
			if result.URLID != 0 {
				if err := store.MarkURLDone(context.Background(), result.URLID); err != nil {
					log.Printf("state update error: %v", err)
				}
			}
			written++
		}
	}
	if dupes > 0 {
		log.Printf("dedup: %d duplicate cid/place_id/data_id records discarded", dupes)
	}

	log.Printf("run complete: written=%d failed=%d", written, failed)

	runErr := <-errCh
	currentJobMu.Lock()
	finalJobID := currentJobID
	currentJobMu.Unlock()
	if runErr == nil && finalJobID != "" {
		if stats, err := store.JobStats(context.Background(), finalJobID); err == nil && stats.Pending == 0 && stats.InProgress == 0 {
			if stats.Failed > 0 {
				_ = store.SetJobStatus(context.Background(), finalJobID, gmaps.JobStatusFailed, nil)
			} else {
				_ = store.SetJobStatus(context.Background(), finalJobID, gmaps.JobStatusDone, nil)
			}
		}
	}
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		if errors.Is(runErr, gmaps.ErrSessionBlocked) {
			currentJobMu.Lock()
			id := currentJobID
			currentJobMu.Unlock()
			log.Printf("session blocked by Google — resume with: --job %s", id)
		} else if errors.Is(runErr, gmaps.ErrJobPaused) {
			log.Printf("job paused — resume with: --job %s", finalJobID)
		} else {
			log.Printf("scraper error: %v", runErr)
		}
	}

	if err := w.Close(); err != nil {
		log.Printf("writer close error: %v", err)
	}
}

func outputMode(dsn string) string {
	if dsn != "" {
		return "database"
	}
	return "file"
}

func languagePostgresTables(lang string) (string, string) {
	suffix := postgresLanguageSuffix(lang)
	return "restaurants_" + suffix, "restaurant_reviews_" + suffix
}

func postgresLanguageSuffix(lang string) string {
	lang = strings.ToLower(strings.TrimSpace(lang))
	re := regexp.MustCompile(`[^a-z0-9]+`)
	suffix := strings.Trim(re.ReplaceAllString(lang, "_"), "_")
	if suffix == "" {
		return "en"
	}
	return suffix
}

type placeDeduper struct {
	cids     map[string]struct{}
	placeIDs map[string]struct{}
	dataIDs  map[string]struct{}
}

func newPlaceDeduper() *placeDeduper {
	return &placeDeduper{
		cids:     make(map[string]struct{}),
		placeIDs: make(map[string]struct{}),
		dataIDs:  make(map[string]struct{}),
	}
}

func (d *placeDeduper) Seen(entry *gmaps.Entry) bool {
	if entry.Cid != "" {
		if _, ok := d.cids[entry.Cid]; ok {
			return true
		}
	}
	if entry.PlaceID != "" {
		if _, ok := d.placeIDs[entry.PlaceID]; ok {
			return true
		}
	}
	if entry.DataID != "" {
		if _, ok := d.dataIDs[entry.DataID]; ok {
			return true
		}
	}

	if entry.Cid != "" {
		d.cids[entry.Cid] = struct{}{}
	}
	if entry.PlaceID != "" {
		d.placeIDs[entry.PlaceID] = struct{}{}
	}
	if entry.DataID != "" {
		d.dataIDs[entry.DataID] = struct{}{}
	}

	return false
}

// runURLsOnly collects feed URLs for all queries and writes them one-per-line
// to outFile. No place detail scraping is performed.
func runURLsOnly(ctx context.Context, outFile, queries string, depth int, lang, geo string) {
	br, err := browser.New(browser.Options{Concurrency: 1, Headless: true, Lang: lang})
	if err != nil {
		log.Fatalf("browser init: %v", err)
	}
	defer br.Close()

	feedOpts := gmaps.FeedOptions{MaxDepth: depth, LangCode: lang, Geo: geo}

	f, err := os.Create(outFile)
	if err != nil {
		log.Fatalf("create output file: %v", err)
	}
	defer f.Close()

	qs := strings.Split(queries, ",")
	var total int
	for i, q := range qs {
		q = strings.TrimSpace(q)
		log.Printf("Query %d/%d %q — collecting URLs", i+1, len(qs), q)
		page := br.AcquirePage()
		urls, err := gmaps.ScrapeFeed(ctx, page, q, feedOpts)
		br.ReleasePage(page)
		if err != nil {
			log.Printf("Query %d/%d %q — feed error: %v", i+1, len(qs), q, err)
			continue
		}
		for _, u := range urls {
			fmt.Fprintln(f, u)
		}
		log.Printf("Query %d/%d %q — %d URLs", i+1, len(qs), q, len(urls))
		total += len(urls)
	}
	log.Printf("Done: %d URLs written to %s", total, outFile)
}
