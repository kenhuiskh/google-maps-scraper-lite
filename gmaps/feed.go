package gmaps

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// PlaceIDToURL converts a Google Maps place ID to a direct place detail URL.
func PlaceIDToURL(placeID string) string {
	q := url.Values{}
	q.Set("api", "1")
	q.Set("query", "place_id:"+placeID)
	q.Set("query_place_id", placeID)
	return "https://www.google.com/maps/search/?" + q.Encode()
}

// scrapePlaceID navigates the query_place_id URL for a single place ID and
// returns the canonical /maps/place/ URL once Google redirects to it. Unlike a
// search-text query, the query_place_id parameter makes Google resolve the place
// reliably, so this skips the feed-selector wait entirely.
func scrapePlaceID(ctx context.Context, page Page, id string) ([]string, error) {
	target := PlaceIDToURL(id)

	status, err := page.Goto(target)
	if err != nil {
		return nil, fmt.Errorf("goto place-ID URL: %w", err)
	}
	if berr := detectBotBlock(page, status); berr != nil {
		return nil, berr
	}

	clickRejectCookiesPlaywright(page)

	if waitUntilURLContainsPlaywright(ctx, page, "/maps/place/", 15*time.Second) {
		return []string{page.URL()}, nil
	}

	return nil, fmt.Errorf("place-ID did not redirect to a place page within timeout")
}

// FeedOptions configures the feed scrape behaviour.
type FeedOptions struct {
	// MaxDepth controls how many scroll iterations are attempted.
	MaxDepth int
	// LangCode is the hl= parameter passed to Google Maps (e.g. "en").
	LangCode string
	// Geo is an optional "@lat,lng,zoom" suffix for geographic targeting
	// (e.g. "43.6532,-79.3832,14z").
	Geo string
}

// ScrapeFeed navigates to the Google Maps search feed for query, scrolls it to
// the configured depth, and returns a deduplicated slice of place URLs found in
// the feed.
func ScrapeFeed(ctx context.Context, page Page, query string, opts FeedOptions) ([]string, error) {
	if id, ok := strings.CutPrefix(query, "place_id:"); ok {
		return scrapePlaceID(ctx, page, id)
	}

	feedStarted := time.Now()
	defer func() {
		logStageTiming("feed.total", feedStarted)
	}()

	fullURL := buildFeedURL(query, opts)

	stageStarted := time.Now()
	status, err := page.Goto(fullURL)
	logStageTiming("feed.goto", stageStarted)
	if err != nil {
		return nil, fmt.Errorf("goto feed URL: %w", err)
	}
	if berr := detectBotBlock(page, status); berr != nil {
		return nil, berr
	}

	clickRejectCookiesPlaywright(page)

	// When Google Maps finds only 1 result it redirects straight to the place page.
	// Poll the URL for ~3 seconds to detect that case.
	const feedSelector = `div[role='feed']`

	stageStarted = time.Now()
	waitErr := page.WaitSelector(feedSelector, 10*time.Second)
	logStageTiming("feed.wait_selector", stageStarted)
	var singlePlace bool

	if waitErr != nil {
		// Feed did not appear — check whether we were redirected to a single place.
		singlePlace = waitUntilURLContainsPlaywright(ctx, page, "/maps/place/", 3*time.Second)
	}

	if singlePlace {
		return []string{page.URL()}, nil
	}

	if waitErr != nil {
		// Neither feed nor single-place redirect — return the error.
		return nil, fmt.Errorf("wait for feed selector: %w", waitErr)
	}

	// Scroll the feed.
	stageStarted = time.Now()
	scrollErr := scrollFeed(ctx, page, opts.MaxDepth, feedSelector)
	logStageTiming("feed.scroll", stageStarted)
	if scrollErr != nil {
		return nil, fmt.Errorf("scroll feed: %w", scrollErr)
	}

	// Parse the page HTML and extract place URLs.
	stageStarted = time.Now()
	content, err := page.Content()
	logStageTiming("feed.page_content", stageStarted)
	if err != nil {
		return nil, fmt.Errorf("get page content: %w", err)
	}

	stageStarted = time.Now()
	urls, err := extractPlaceURLs(content)
	logStageTiming("feed.extract_urls", stageStarted)
	if err != nil {
		return nil, fmt.Errorf("extract place URLs: %w", err)
	}

	return urls, nil
}

// buildFeedURL constructs the Google Maps search URL for the given query and options.
func buildFeedURL(query string, opts FeedOptions) string {
	escaped := url.QueryEscape(query)

	var base string

	if opts.Geo != "" {
		geo := strings.ReplaceAll(opts.Geo, " ", "")
		base = fmt.Sprintf("https://www.google.com/maps/search/%s/@%s", escaped, geo)
	} else {
		base = fmt.Sprintf("https://www.google.com/maps/search/%s", escaped)
	}

	if opts.LangCode != "" {
		base += "?hl=" + url.QueryEscape(opts.LangCode)
	}

	return base
}

// clickRejectCookiesPlaywright runs the consent/cookie-rejection JavaScript.
// Errors are intentionally ignored — the page may not have a cookie banner.
func clickRejectCookiesPlaywright(page Page) {
	_, _ = page.Evaluate(`() => {
		// Try consent form buttons first
		const consentForm = document.querySelector('form[action*="consent.google"]');
		if (consentForm) {
			const btn = consentForm.querySelector('button, input[type="submit"]');
			if (btn) {
				btn.click();
				return true;
			}
		}
		// Try reject/decline buttons
		const buttons = document.querySelectorAll('button, input[type="submit"]');
		for (const btn of buttons) {
			const text = (btn.textContent || btn.value || '').toLowerCase();
			if (text.includes('reject') || text.includes('decline') || text.includes('ablehnen')) {
				btn.click();
				return true;
			}
		}
		return false;
	}`)
}

// waitUntilURLContainsPlaywright polls page.URL() until it contains s or the
// deadline is reached.
func waitUntilURLContainsPlaywright(ctx context.Context, page Page, s string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			if strings.Contains(page.URL(), s) {
				return true
			}
			if time.Now().After(deadline) {
				return false
			}
		}
	}
}

// scrollFeed scrolls the feed element repeatedly up to maxDepth times.
func scrollFeed(ctx context.Context, page Page, maxDepth int, scrollSelector string) error {
	expr := `async () => {
		const el = document.querySelector("` + scrollSelector + `");
		el.scrollTop = el.scrollHeight;

		return new Promise((resolve, reject) => {
			setTimeout(() => {
			resolve(el.scrollHeight);
			}, %d);
		});
	}`

	var currentScrollHeight int

	waitTime := 100.0

	const (
		timeout  = 500
		maxWait2 = 2000
	)

	for i := 0; i < maxDepth; i++ {
		cnt := i + 1
		waitTime2 := timeout * cnt

		if waitTime2 > maxWait2 {
			waitTime2 = maxWait2
		}

		result, err := page.Evaluate(fmt.Sprintf(expr, waitTime2))
		if err != nil {
			return fmt.Errorf("scroll evaluate (iter %d): %w", i, err)
		}

		var height int

		switch v := result.(type) {
		case int:
			height = v
		case float64:
			height = int(v)
		default:
			return fmt.Errorf("scrollHeight is not a number, got %T", result)
		}

		if height == currentScrollHeight {
			break
		}

		currentScrollHeight = height

		select {
		case <-ctx.Done():
			return nil
		default:
		}

		page.Sleep(time.Duration(waitTime) * time.Millisecond)

		waitTime *= 1.5
		if waitTime > maxWait2 {
			waitTime = maxWait2
		}
	}

	return nil
}

// extractPlaceURLs parses the HTML content and returns deduplicated place URLs.
func extractPlaceURLs(content string) ([]string, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(content))
	if err != nil {
		return nil, fmt.Errorf("parse HTML: %w", err)
	}

	feed := doc.Find(`div[role="feed"]`).First()
	if feed.Length() == 0 {
		return nil, fmt.Errorf("feed container not found")
	}

	seen := make(map[string]struct{})
	var urls []string

	feed.Find(`div[jsaction] > a[href]`).Each(func(_ int, s *goquery.Selection) {
		href := s.AttrOr("href", "")
		if href == "" || !strings.Contains(href, "/maps/place/") {
			return
		}

		// Strip tab/panel specifiers (e.g. !10e2 = hours tab) before deduplication
		// so hours-specific links are normalised to the main overview URL.
		href = mapsTabRE.ReplaceAllString(href, "")

		if _, exists := seen[href]; !exists {
			seen[href] = struct{}{}
			urls = append(urls, href)
		}
	})

	if len(urls) == 0 {
		return nil, fmt.Errorf("feed container found but no place URLs extracted (possible DOM drift or selector mismatch)")
	}

	return urls, nil
}
