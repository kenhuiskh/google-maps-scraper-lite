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
	"strings"
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

	queries := flag.String("queries", "", "comma-separated search queries")
	resume := flag.String("resume", "", "path to checkpoint file to resume a blocked run (skips feed phase)")
	concurrency := flag.Int("c", 1, "concurrency level")
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

	if *errorLog != "" {
		f, err := os.OpenFile(*errorLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			log.Fatalf("open error log: %v", err)
		}
		defer f.Close()
		log.SetOutput(io.MultiWriter(os.Stderr, f))
	}

	if *resume == "" && *queries == "" {
		fmt.Fprintln(os.Stderr, "error: -queries is required (or use -resume to continue a previous run)")
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

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if *urlsOnly != "" {
		runURLsOnly(ctx, *urlsOnly, *queries, *depth, *lang, *geo)
		return
	}

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

	// Ensure output dir exists (needed for both checkpoint and file output).
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		log.Fatalf("create output dir: %v", err)
	}

	// Resolve checkpoint path: use the resume file if provided, otherwise create
	// a new one stamped with the current run's timestamp in the output directory.
	checkpointPath := filepath.Join(*outDir, ts+".checkpoint.json")
	if *resume != "" {
		checkpointPath = *resume
	}

	var w output.Writer
	switch {
	case *dsn != "":
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

	out := make(chan *gmaps.Entry, *concurrency*2)

	s := gmaps.Scraper{
		Config: gmaps.Config{
			Concurrency:    *concurrency,
			Depth:          *depth,
			Lang:           *lang,
			Geo:            *geo,
			Radius:         *radius,
			ExtractEmail:   *extractEmail,
			ExtraReviews:   *extraReviews,
			Limit:          *limit,
			CheckpointPath: checkpointPath,
		},
		Pool: br,
	}

	// Build query list: from checkpoint when resuming, from flag otherwise.
	var qs []string
	if *resume != "" {
		cp, err := gmaps.LoadCheckpoint(*resume)
		if err != nil {
			log.Fatalf("load checkpoint: %v", err)
		}
		qs = cp.Queries
		log.Printf("Resuming checkpoint %s: %d URLs, %d already done", *resume, len(cp.URLs), len(cp.Done))
	} else {
		qs = strings.Split(*queries, ",")
		for i, q := range qs {
			qs[i] = strings.TrimSpace(q)
		}
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.Run(ctx, qs, out)
	}()

	var written, failed, dupes int
	seenPlaceIDs := make(map[string]struct{})
	for entry := range out {
		if entry.PlaceID != "" {
			if _, seen := seenPlaceIDs[entry.PlaceID]; seen {
				dupes++
				continue
			}
			seenPlaceIDs[entry.PlaceID] = struct{}{}
		}
		if err := w.Write(entry); err != nil {
			log.Printf("write error: %v", err)
			failed++
		} else {
			written++
		}
	}
	if dupes > 0 {
		log.Printf("dedup: %d duplicate place_id records discarded", dupes)
	}

	log.Printf("run complete: written=%d failed=%d", written, failed)

	if err := <-errCh; err != nil && !errors.Is(err, context.Canceled) {
		if errors.Is(err, gmaps.ErrSessionBlocked) {
			log.Printf("session blocked by Google — resume with: --resume %s", checkpointPath)
		} else {
			log.Printf("scraper error: %v", err)
		}
	}

	if err := w.Close(); err != nil {
		log.Printf("writer close error: %v", err)
	}
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
