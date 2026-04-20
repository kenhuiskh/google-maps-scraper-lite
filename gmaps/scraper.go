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
	Concurrency  int
	Depth        int
	Lang         string
	Geo          string
	Radius       float64 // meters; 0 = no filter
	ExtractEmail bool
	ExtraReviews int
	Limit        int // max places to scrape; 0 = no limit
}

// PagePool provides playwright pages to workers.
type PagePool interface {
	AcquirePage() playwright.Page
	ReleasePage(playwright.Page)
}

// Scraper orchestrates the full scraping pipeline.
type Scraper struct {
	Config Config
	Pool   PagePool
}

// Run scrapes all queries and sends results to out. Run closes out when done.
// The caller must drain out concurrently or Run will deadlock.
func (s *Scraper) Run(ctx context.Context, queries []string, out chan<- *Entry) error {
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

	// Collect all place URLs first (serially, one feed page per query)
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

	// Fan-out place detail scraping
	urlsCh := make(chan string, len(placeURLs))
	for _, u := range placeURLs {
		urlsCh <- u
	}
	close(urlsCh)

	conc := s.Config.Concurrency
	if conc < 1 {
		conc = 1
	}

	useRadiusFilter := s.Config.Radius > 0 && s.Config.Geo != ""

	var (
		mu      sync.Mutex
		entries []*Entry // only populated when useRadiusFilter
	)
	var completed int64

	g, gctx := errgroup.WithContext(ctx)
	for i := 0; i < conc; i++ {
		workerID := i
		g.Go(func() error {
			rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(workerID)))
			for placeURL := range urlsCh {
				time.Sleep(time.Duration(rng.Intn(1000)) * time.Millisecond)

				page := s.Pool.AcquirePage()
				entry, err := ScrapePlace(gctx, page, placeURL, placeOpts)
				for retry := 1; retry <= 2 && err != nil && strings.Contains(err.Error(), "Page crashed"); retry++ {
					s.Pool.ReleasePage(page)
					time.Sleep(time.Duration(retry*2) * time.Second)
					page = s.Pool.AcquirePage()
					entry, err = ScrapePlace(gctx, page, placeURL, placeOpts)
				}
				s.Pool.ReleasePage(page)
				if err != nil {
					log.Printf("place scrape error %s: %v", placeURL, err)
					continue
				}
				done := atomic.AddInt64(&completed, 1)
				if done%10 == 0 {
					log.Printf("Places %d/%d completed", done, int64(len(placeURLs)))
				}
				if useRadiusFilter {
					mu.Lock()
					entries = append(entries, entry)
					mu.Unlock()
				} else {
					select {
					case out <- entry:
					case <-gctx.Done():
						return gctx.Err()
					}
				}
			}
			return nil
		})
	}

	err := g.Wait()

	if useRadiusFilter && (err == nil || errors.Is(err, context.Canceled)) {
		lat, lon, _ := ParseGeoCenter(s.Config.Geo) // already validated by caller
		filtered := filterAndSortEntriesWithinRadius(entries, lat, lon, s.Config.Radius)
		for _, e := range filtered {
			select {
			case out <- e:
			case <-ctx.Done():
				goto done
			}
		}
	}

done:
	close(out)
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
