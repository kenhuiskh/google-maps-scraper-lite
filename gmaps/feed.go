package gmaps

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/playwright-community/playwright-go"
)

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
func ScrapeFeed(ctx context.Context, page playwright.Page, query string, opts FeedOptions) ([]string, error) {
	fullURL := buildFeedURL(query, opts)

	if _, err := page.Goto(fullURL, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
	}); err != nil {
		return nil, fmt.Errorf("goto feed URL: %w", err)
	}

	clickRejectCookiesPlaywright(page)

	// When Google Maps finds only 1 result it redirects straight to the place page.
	// Poll the URL for ~3 seconds to detect that case.
	const feedSelector = `div[role='feed']`

	_, waitErr := page.WaitForSelector(feedSelector, playwright.PageWaitForSelectorOptions{
		Timeout: playwright.Float(10000),
	})

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
	if err := scrollFeed(ctx, page, opts.MaxDepth, feedSelector); err != nil {
		return nil, fmt.Errorf("scroll feed: %w", err)
	}

	// Parse the page HTML and extract place URLs.
	content, err := page.Content()
	if err != nil {
		return nil, fmt.Errorf("get page content: %w", err)
	}

	urls, err := extractPlaceURLs(content)
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
func clickRejectCookiesPlaywright(page playwright.Page) {
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
func waitUntilURLContainsPlaywright(ctx context.Context, page playwright.Page, s string, timeout time.Duration) bool {
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
func scrollFeed(ctx context.Context, page playwright.Page, maxDepth int, scrollSelector string) error {
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

		page.WaitForTimeout(waitTime)

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
		if href == "" {
			return
		}

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
