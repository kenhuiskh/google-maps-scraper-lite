package gmaps

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"log"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var (
	// mapsTabRE matches the !10eN segment in Google Maps data= URLs that forces a
	// specific panel/tab view (e.g. hours tab). Stripping it loads the main overview.
	mapsTabRE = regexp.MustCompile(`!10e\d+`)

	daysAgoRE   = regexp.MustCompile(`(?i)^(\d+)\s+days?\s+ago$`)
	weeksAgoRE  = regexp.MustCompile(`(?i)^(\d+)\s+weeks?\s+ago$`)
	aWeekAgoRE  = regexp.MustCompile(`(?i)^a\s+week\s+ago$`)
	monthsAgoRE = regexp.MustCompile(`(?i)^(\d+)\s+months?\s+ago$`)
	aMonthAgoRE = regexp.MustCompile(`(?i)^a\s+month\s+ago$`)
	yearsAgoRE  = regexp.MustCompile(`(?i)^(\d+)\s+years?\s+ago$`)
	aYearAgoRE  = regexp.MustCompile(`(?i)^a\s+year\s+ago$`)
	todayAgoRE  = regexp.MustCompile(`(?i)^(just now|today)$`)
	pageTitleRE = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
)

// PlaceOptions configures the place detail scrape behaviour.
type PlaceOptions struct {
	// ExtractEmail controls whether emails are extracted from the place's website.
	ExtractEmail bool
	// LangCode is the hl= parameter passed to Google Maps (e.g. "en").
	LangCode string
	// ExtraReviews fetches additional review pages to fill up to the requested review count.
	ExtraReviews int
}

// ScrapePlace navigates to placeURL, extracts the place's JSON data, parses it
// into an Entry, and optionally extracts email addresses from the place's website.
func ScrapePlace(ctx context.Context, page Page, placeURL string, opts PlaceOptions) (*Entry, error) {
	placeStarted := time.Now()
	defer func() {
		logStageTiming("place.total", placeStarted)
	}()

	fullURL := placeURLWithLang(placeURL, opts.LangCode)

	finishStage := tracePlaceStage(ctx, "place.goto")
	status, err := page.Goto(fullURL)
	finishStage()
	if err != nil {
		return nil, fmt.Errorf("goto place URL: %w", err)
	}

	finishStage = tracePlaceStage(ctx, "place.bot_check")
	berr := detectBotBlock(page, status)
	finishStage()
	if berr != nil {
		return nil, berr
	}

	finishStage = tracePlaceStage(ctx, "place.cookies")
	clickRejectCookiesPlaywright(page)
	finishStage()

	finishStage = tracePlaceStage(ctx, "place.wait_rich")
	canonicalURL := waitForRichPlacePage(ctx, page)
	finishStage()

	finishStage = tracePlaceStage(ctx, "place.expand_hours")
	expandOpeningHours(page)
	finishStage()

	finishStage = tracePlaceStage(ctx, "place.extract_json")
	raw, err := extractPlaceJSON(ctx, page)
	finishStage()
	if err != nil {
		return nil, fmt.Errorf("extract place JSON: %w", err)
	}

	finishStage = tracePlaceStage(ctx, "place.parse_json")
	entry, err := EntryFromJSON(raw)
	finishStage()
	if err != nil {
		return nil, fmt.Errorf("parse entry JSON: %w", err)
	}

	finishStage = tracePlaceStage(ctx, "place.review_tags")
	entry.ReviewTags = extractReviewTags(page)
	finishStage()

	entry.Link = choosePlaceLink(entry.Link, canonicalURL, fullURL)

	if opts.ExtraReviews > 0 {
		finishStage = tracePlaceStageDetail(ctx, "place.extra_reviews", fmt.Sprintf("target=%d", opts.ExtraReviews))
		if err := scrapeExtraReviews(ctx, page, &entry, opts.ExtraReviews); err != nil {
			log.Printf("extra reviews scrape warning for %s: %v", placeURL, err)
		}
		finishStage()
		entry.SortAndCapReviews(opts.ExtraReviews)
	}

	if opts.ExtractEmail && entry.IsWebsiteValidForEmail() {
		finishStage = tracePlaceStage(ctx, "place.email")
		websiteURL := normalizeGoogleURL(entry.WebSite)
		emails, err := ExtractEmails(ctx, websiteURL)
		finishStage()
		if err == nil {
			entry.Emails = emails
		}
	}

	return &entry, nil
}

// detectBotBlock returns a non-nil error wrapping ErrBotBlocked when the page
// shows a Google captcha/consent/sorry wall, a 429 response, or "unusual
// traffic" / "automated queries" text. Returns nil otherwise.
func detectBotBlock(page Page, status int) error {
	curURL := page.URL()
	pageClass := classifyPageMetadata(curURL, status, "")
	for _, marker := range []string{"/sorry/", "consent.google", "ipv4.google.com/sorry"} {
		if strings.Contains(curURL, marker) {
			observePageMetadata(page, pageClass, "", "")
			return fmt.Errorf("bot block: url %q: %w", curURL, ErrBotBlocked)
		}
	}
	if status == 429 {
		observePageMetadata(page, "rate_limited", "", "")
		return fmt.Errorf("bot block: HTTP 429: %w", ErrBotBlocked)
	}
	if content, err := page.Content(); err == nil {
		lc := strings.ToLower(content)
		pageClass = classifyPageMetadata(curURL, status, lc)
		title := ""
		if match := pageTitleRE.FindStringSubmatch(content); len(match) == 2 {
			title = strings.Join(strings.Fields(html.UnescapeString(match[1])), " ")
		}
		observePageMetadata(page, pageClass, content, title)
		if strings.Contains(lc, "unusual traffic") || strings.Contains(lc, "automated queries") {
			return fmt.Errorf("bot block: unusual-traffic page: %w", ErrBotBlocked)
		}
	} else {
		observePageMetadata(page, pageClass, "", "")
	}
	return nil
}

func classifyPageMetadata(curURL string, status int, lowerContent string) string {
	lowerURL := strings.ToLower(curURL)
	switch {
	case status == 429:
		return "rate_limited"
	case strings.Contains(lowerURL, "/sorry/"), strings.Contains(lowerURL, "ipv4.google.com/sorry"):
		return "sorry"
	case strings.Contains(lowerURL, "consent.google"), strings.Contains(lowerContent, "before you continue"):
		return "consent"
	case strings.Contains(lowerContent, "recaptcha"), strings.Contains(lowerContent, "captcha"):
		return "captcha"
	case strings.Contains(lowerContent, "unusual traffic"), strings.Contains(lowerContent, "automated queries"):
		return "unusual_traffic"
	case strings.Contains(lowerURL, "/maps/"), strings.Contains(lowerContent, "app_initialization_state"):
		return "maps"
	default:
		return "unknown"
	}
}

func observePageMetadata(page Page, class, content, title string) {
	if observer, ok := page.(PageDiagnosticObserver); ok {
		observer.ObservePageDiagnostics(class, len(content), title)
	}
}

func placeURLWithLang(placeURL, lang string) string {
	fullURL := mapsTabRE.ReplaceAllString(placeURL, "")
	if lang == "" {
		return fullURL
	}

	parsed, err := url.Parse(fullURL)
	if err != nil {
		sep := "?"
		if strings.Contains(fullURL, "?") {
			sep = "&"
		}
		return fullURL + sep + "hl=" + url.QueryEscape(lang)
	}

	q := parsed.Query()
	q.Set("hl", lang)
	parsed.RawQuery = q.Encode()
	return parsed.String()
}

func waitForRichPlacePage(ctx context.Context, page Page) string {
	canonicalURL := waitForPlaceIDResolution(ctx, page, 20*time.Second)

	selectors := []string{
		`h1`,
		`[data-item-id="oh"]`,
		`[aria-label="Refine reviews"]`,
		`[role="main"]`,
	}
	for _, sel := range selectors {
		if err := page.WaitSelector(sel, 2500*time.Millisecond); err == nil {
			return canonicalURL
		}
	}

	return canonicalURL
}

func waitForPlaceIDResolution(ctx context.Context, page Page, timeout time.Duration) string {
	if canonical := canonicalPlaceURL(page.URL()); canonical != "" && !isPlaceIDQueryURL(page.URL()) {
		return canonical
	}
	if !isPlaceIDQueryURL(page.URL()) {
		return ""
	}
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ""
		case <-ticker.C:
			if canonical := canonicalPlaceURL(page.URL()); canonical != "" && !isPlaceIDQueryURL(page.URL()) {
				return canonical
			}
			if time.Now().After(deadline) {
				return ""
			}
		}
	}
}

func isPlaceIDQueryURL(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return strings.Contains(rawURL, "place_id:") || strings.Contains(rawURL, "query_place_id=")
	}
	q := parsed.Query()
	return strings.HasPrefix(q.Get("q"), "place_id:") ||
		strings.HasPrefix(q.Get("query"), "place_id:") ||
		q.Get("query_place_id") != ""
}

func choosePlaceLink(parsedLink, canonicalURL, fallbackURL string) string {
	if canonical := canonicalPlaceURL(canonicalURL); canonical != "" {
		return canonical
	}
	if parsedLink != "" && !mapsTabRE.MatchString(parsedLink) && !isPlaceIDQueryURL(parsedLink) {
		return parsedLink
	}
	if canonical := canonicalPlaceURL(fallbackURL); canonical != "" {
		return canonical
	}
	return fallbackURL
}

func canonicalPlaceURL(rawURL string) string {
	if rawURL == "" || isPlaceIDQueryURL(rawURL) {
		return ""
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		if strings.Contains(rawURL, "/maps/place/") {
			return rawURL
		}
		return ""
	}
	if !strings.Contains(parsed.Path, "/maps/place/") {
		return ""
	}
	return rawURL
}

// scrapeExtraReviews opens the reviews dialog, sorts by newest, scrolls the DOM
// feed, and extracts visible review cards until entry has at least targetReviews
// reviews or scrolling stops revealing new cards.
func scrapeExtraReviews(ctx context.Context, page Page, entry *Entry, targetReviews int) error {
	const (
		maxStaleScrolls = 3
		scrollPauseMs   = 1800
	)

	// Click the "All reviews" / reviews tab to open the reviews panel.
	reviewSelectors := []string{
		`button[aria-label*="review" i][jsaction]`,
		`button[aria-label*="reviews" i]`,
		`[data-tab-index="1"]`,
		`a[href*="reviews"]`,
	}
	updatePlaceStageDetail(ctx, "place.extra_reviews", fmt.Sprintf("action=open target=%d", targetReviews))
	for _, sel := range reviewSelectors {
		if err := page.ClickForce(sel, 3000*time.Millisecond, 2000*time.Millisecond); err != nil {
			continue
		}
		page.Sleep(scrollPauseMs * time.Millisecond)
		break
	}

	_ = page.WaitSelector("[data-review-id], .jftiEf, .wiI7pd", 5000*time.Millisecond)

	// Sort by "Newest". In current Maps UI this usually requires opening the sort
	// menu first, then clicking the "Newest" option.
	sortButtonSelectors := []string{
		`button[aria-label*="Sort reviews" i]`,
		`button[aria-label*="Sort" i]`,
		`[data-value="Sort reviews"]`,
	}
	updatePlaceStageDetail(ctx, "place.extra_reviews", fmt.Sprintf("action=open_sort target=%d", targetReviews))
	for _, sel := range sortButtonSelectors {
		if err := page.ClickForce(sel, 3000*time.Millisecond, 2000*time.Millisecond); err != nil {
			continue
		}
		page.Sleep(scrollPauseMs * time.Millisecond)
		break
	}

	newestSelectors := []string{
		`button[aria-label*="Newest" i]`,
		`[role="menuitemradio"][aria-label*="Newest" i]`,
		`[data-index="1"]`,
		`[data-value="2"]`,
	}
	updatePlaceStageDetail(ctx, "place.extra_reviews", fmt.Sprintf("action=select_newest target=%d", targetReviews))
	for _, sel := range newestSelectors {
		if err := page.ClickForce(sel, 3000*time.Millisecond, 2000*time.Millisecond); err != nil {
			continue
		}
		page.Sleep(scrollPauseMs * time.Millisecond)
		break
	}

	reviewKey := func(r Review) string {
		return strings.TrimSpace(r.Name) + "|" + strings.TrimSpace(r.When) + "|" + strings.TrimSpace(r.Description)
	}

	seen := make(map[string]bool, len(entry.UserReviews)+len(entry.UserReviewsExtended)+targetReviews)
	for _, review := range entry.UserReviews {
		seen[reviewKey(review)] = true
	}
	for _, review := range entry.UserReviewsExtended {
		seen[reviewKey(review)] = true
	}

	// Scroll the reviews panel until target is met or stale.
	staleCount := 0
	scrollCount := 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		updatePlaceStageDetail(ctx, "place.extra_reviews", fmt.Sprintf(
			"action=extract scroll=%d collected=%d target=%d stale=%d",
			scrollCount, len(entry.UserReviews)+len(entry.UserReviewsExtended), targetReviews, staleCount,
		))
		reviews, err := extractDOMReviews(page)
		if err != nil {
			return err
		}

		newCount := 0
		for _, review := range reviews {
			key := reviewKey(review)
			if seen[key] {
				continue
			}
			seen[key] = true
			entry.UserReviewsExtended = append(entry.UserReviewsExtended, review)
			newCount++
		}

		if len(entry.UserReviews)+len(entry.UserReviewsExtended) >= targetReviews {
			break
		}

		// Scroll the reviews panel.
		scrollCount++
		updatePlaceStageDetail(ctx, "place.extra_reviews", fmt.Sprintf(
			"action=scroll scroll=%d collected=%d target=%d stale=%d",
			scrollCount, len(entry.UserReviews)+len(entry.UserReviewsExtended), targetReviews, staleCount,
		))
		_, _ = page.Evaluate(scrollReviewsFeedJS)
		page.Sleep(scrollPauseMs * time.Millisecond)

		if newCount == 0 {
			staleCount++
			if staleCount >= maxStaleScrolls {
				break
			}
		} else {
			staleCount = 0
		}
	}

	return nil
}

func extractDOMReviews(page Page) ([]Review, error) {
	result, err := page.Evaluate(extractDOMReviewsJS)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return []Review{}, nil
	}

	b, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}

	type domReview struct {
		Name   string `json:"name"`
		Rating int    `json:"rating"`
		Date   string `json:"date"`
		Text   string `json:"text"`
		PicURL string `json:"pic_url"`
	}

	var rawReviews []domReview
	if err := json.Unmarshal(b, &rawReviews); err != nil {
		return nil, err
	}

	reviews := make([]Review, 0, len(rawReviews))
	for _, r := range rawReviews {
		review := Review{
			Name:           strings.TrimSpace(r.Name),
			Rating:         r.Rating,
			Description:    strings.TrimSpace(r.Text),
			ProfilePicture: strings.TrimSpace(r.PicURL),
			When:           relativeToAbsoluteDate(r.Date),
		}
		if review.Name == "" {
			continue
		}
		reviews = append(reviews, review)
	}

	return reviews, nil
}

func relativeToAbsoluteDate(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}

	now := time.Now()

	switch {
	case todayAgoRE.MatchString(s):
		return fmt.Sprintf("%d-%d-%d", now.Year(), int(now.Month()), now.Day())
	case aWeekAgoRE.MatchString(s):
		t := now.AddDate(0, 0, -7)
		return fmt.Sprintf("%d-%d-%d", t.Year(), int(t.Month()), t.Day())
	case aMonthAgoRE.MatchString(s):
		t := now.AddDate(0, -1, 0)
		return fmt.Sprintf("%d-%d-%d", t.Year(), int(t.Month()), t.Day())
	case aYearAgoRE.MatchString(s):
		t := now.AddDate(-1, 0, 0)
		return fmt.Sprintf("%d-%d-%d", t.Year(), int(t.Month()), t.Day())
	}

	if matches := daysAgoRE.FindStringSubmatch(s); len(matches) == 2 {
		var days int
		_, _ = fmt.Sscanf(matches[1], "%d", &days)
		t := now.AddDate(0, 0, -days)
		return fmt.Sprintf("%d-%d-%d", t.Year(), int(t.Month()), t.Day())
	}
	if matches := weeksAgoRE.FindStringSubmatch(s); len(matches) == 2 {
		var weeks int
		_, _ = fmt.Sscanf(matches[1], "%d", &weeks)
		t := now.AddDate(0, 0, -(weeks * 7))
		return fmt.Sprintf("%d-%d-%d", t.Year(), int(t.Month()), t.Day())
	}
	if matches := monthsAgoRE.FindStringSubmatch(s); len(matches) == 2 {
		var months int
		_, _ = fmt.Sscanf(matches[1], "%d", &months)
		t := now.AddDate(0, -months, 0)
		return fmt.Sprintf("%d-%d-%d", t.Year(), int(t.Month()), t.Day())
	}
	if matches := yearsAgoRE.FindStringSubmatch(s); len(matches) == 2 {
		var years int
		_, _ = fmt.Sscanf(matches[1], "%d", &years)
		t := now.AddDate(-years, 0, 0)
		return fmt.Sprintf("%d-%d-%d", t.Year(), int(t.Month()), t.Day())
	}

	return ""
}

// extractPlaceJSON retries up to 3 times to extract the APP_INITIALIZATION_STATE
// JSON from the current page, reloading between attempts on failure.
func extractPlaceJSON(ctx context.Context, page Page) ([]byte, error) {
	const maxAttempts = 3

	for attempt := range maxAttempts {
		updatePlaceStageDetail(ctx, "place.extract_json", fmt.Sprintf("action=evaluate attempt=%d/%d", attempt+1, maxAttempts))
		if berr := detectBotBlock(page, 0); berr != nil {
			return nil, berr
		}
		raw, err := getRawPlaceJSON(ctx, page)
		if err != nil || raw == nil {
			if attempt < maxAttempts-1 {
				// Brief pause before reload to avoid immediately re-hitting a
				// rate-limited or bot-detected response.
				updatePlaceStageDetail(ctx, "place.extract_json", fmt.Sprintf("action=reload_wait attempt=%d/%d", attempt+1, maxAttempts))
				page.Sleep(time.Duration(2000*(attempt+1)) * time.Millisecond)
				updatePlaceStageDetail(ctx, "place.extract_json", fmt.Sprintf("action=reload attempt=%d/%d", attempt+1, maxAttempts))
				if status, reloadErr := page.Reload(); reloadErr == nil {
					if berr := detectBotBlock(page, status); berr != nil {
						return nil, berr
					}
					continue
				}
			}

			if err != nil {
				return nil, err
			}

			return nil, fmt.Errorf("APP_INITIALIZATION_STATE data not found")
		}

		str, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("could not convert to string, got type %T", raw)
		}

		const prefix = `)]}'`

		str = strings.TrimSpace(strings.TrimPrefix(str, prefix))

		return []byte(str), nil
	}

	return nil, fmt.Errorf("APP_INITIALIZATION_STATE data not found after retries")
}

// getRawPlaceJSON polls page.Evaluate with the APP_INITIALIZATION_STATE JS extractor
// until a non-empty result is returned or ctx is cancelled.
func getRawPlaceJSON(ctx context.Context, page Page) (any, error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	for {
		select {
		case <-timeoutCtx.Done():
			return nil, fmt.Errorf("timeout while getting raw data: %w", timeoutCtx.Err())
		default:
			raw, err := page.Evaluate(placeJS)
			if err != nil {
				page.Sleep(200 * time.Millisecond)
				continue
			}

			if raw == nil {
				page.Sleep(200 * time.Millisecond)
				continue
			}

			if str, ok := raw.(string); ok && str == "" {
				page.Sleep(200 * time.Millisecond)
				continue
			}

			return raw, nil
		}
	}
}

// expandOpeningHours attempts to click the opening hours button to expand the full
// week schedule before JSON extraction. Errors are intentionally ignored — the button
// may not exist (place has no hours, or already expanded).
//
// Uses Force: true on Click to bypass Playwright's actionability checks (visibility,
// viewport position) while still sending a trusted CDP click (isTrusted=true).
func expandOpeningHours(page Page) {
	selectors := []string{
		// Prefer exact data-item-id used for the opening hours row
		`[data-item-id="oh"]`,
		`button[aria-expanded="false"][data-item-id*="oh"]`,
		// aria-expanded variants (collapsed state)
		`button[aria-expanded="false"][aria-label*="hour" i]`,
		`button[aria-expanded="false"][aria-label*="Closes" i]`,
		`button[aria-expanded="false"][aria-label*="Opens" i]`,
		`button[aria-expanded="false"][aria-label*="Open" i]`,
		// div-based button fallback
		`div[role="button"][aria-label*="hour" i]`,
		`div[role="button"][aria-label*="Closes" i]`,
		`div[role="button"][aria-label*="Opens" i]`,
	}

	for _, sel := range selectors {
		// Force: true bypasses Playwright's actionability checks (visibility, viewport position,
		// stability). The click is still sent via CDP and produces isTrusted=true, so Google
		// Maps' React event handlers respond to it — unlike JS dispatchEvent (isTrusted=false).
		if err := page.ClickForce(sel, 250*time.Millisecond, 2000*time.Millisecond); err != nil {
			continue
		}

		// Give the DOM a moment to update before JSON extraction.
		page.Sleep(500 * time.Millisecond)
		return
	}
}

// extractReviewTags reads the "Refine reviews" chip bar from the DOM and returns
// each tag with its mention count. The "All" chip and "View N more Topics" button
// are skipped. Count is nil when no number can be parsed.
func extractReviewTags(page Page) []ReviewTag {
	// By this stage the rich page, opening hours, and JSON state have already had
	// time to hydrate. Keep a short grace period for late review chips, but do not
	// stall every cold tab or place without review tags for several seconds.
	if err := page.WaitSelector(`[aria-label="Refine reviews"]`, 500*time.Millisecond); err != nil {
		return []ReviewTag{}
	}

	result, err := page.Evaluate(reviewTagsJS)
	if err != nil || result == nil {
		return []ReviewTag{}
	}

	// Re-marshal and unmarshal into []ReviewTag to avoid Playwright's uncertain
	// numeric types (int vs float64) from manual type assertions.
	b, err := json.Marshal(result)
	if err != nil {
		return []ReviewTag{}
	}

	var tags []ReviewTag
	if err := json.Unmarshal(b, &tags); err != nil {
		return []ReviewTag{}
	}

	return tags
}

// reviewTagsJS extracts review keyword chips from the "Refine reviews" bar.
// Tag name comes from span.uEubGf. Count is extracted from span.bC3Nkc first;
// if that class is absent (Google Maps layout changes), falls back to finding
// any span inside the button whose text is a pure integer and differs from the tag name.
const reviewTagsJS = `
(function() {
	const container = document.querySelector('[aria-label="Refine reviews"]');
	if (!container) return [];

	const buttons = container.querySelectorAll('button[role="radio"]');
	const tags = [];

	for (const btn of buttons) {
		const label = btn.getAttribute('aria-label') || '';
		if (label === 'All reviews') continue;
		if (/^View \d+ more/.test(label)) continue;

		const nameSpan = btn.querySelector('span.uEubGf');
		const name = nameSpan ? nameSpan.textContent.trim() : '';
		if (!name || name === 'All' || /^\+\d+$/.test(name)) continue;

		const countSpan = btn.querySelector('span.bC3Nkc');
		let countRaw = countSpan ? (countSpan.innerText || countSpan.textContent).trim() : null;

		// Fallback: find any span whose visible text contains a number and isn't the tag name.
		if (!countRaw || !/\d/.test(countRaw)) {
			const spans = btn.querySelectorAll('span');
			for (const s of spans) {
				const t = (s.innerText || s.textContent).trim();
				if (/\d/.test(t) && t !== name) {
					countRaw = t;
					break;
				}
			}
		}

		const countMatch = countRaw ? countRaw.match(/\d+/) : null;
		const count = countMatch ? parseInt(countMatch[0], 10) : null;

		tags.push({ tag: name, count: count });
	}

	return tags;
})()
`

const expandReviewTextJS = `
(function() {
	const selectors = [
		'button[aria-label*=" More" i]',
		'button[aria-label^="More" i]',
		'button.w8nwRe',
		'button[jsaction*="pane.review.expandReview"]'
	];

	for (const sel of selectors) {
		for (const btn of document.querySelectorAll(sel)) {
			try {
				btn.click();
			} catch (_) {}
		}
	}

	return true;
})()
`

const scrollReviewsFeedJS = `
(function() {
	const reviewCard = document.querySelector('[data-review-id], .jftiEf');
	const candidates = [
		'[role="feed"]',
		'.m6QErb.DxyBCb',
		'.m6QErb[tabindex="-1"]',
		reviewCard ? reviewCard.closest('.m6QErb') : null,
		'.m6QErb',
		'[data-reviews-feed]',
		'[role="main"]',
	];

	for (const candidate of candidates) {
		const el = typeof candidate === 'string' ? document.querySelector(candidate) : candidate;
		if (el && el.scrollHeight > el.clientHeight) {
			el.scrollTop += Math.max(1600, el.clientHeight);
			return true;
		}
	}

	window.scrollBy(0, 1800);
	return false;
})()
`

const extractDOMReviewsJS = `
(function() {
	const cards = Array.from(document.querySelectorAll('[data-review-id], .jftiEf'));
	const seen = new Set();

	return cards.map((card) => {
		const nameNode = card.querySelector('.d4r55');
		const ratingNode = card.querySelector('.kvMYJc');
		const dateNode = card.querySelector('.rsqaWe');
		const textNode = card.querySelector('.wiI7pd');
		const imgNode = card.querySelector('img.NBa7we') || card.querySelector('.ogfYpf img');
		const ariaLabel = ratingNode ? (ratingNode.getAttribute('aria-label') || '') : '';
		const match = ariaLabel.match(/(\d+)/);

		return {
			name:    nameNode ? (nameNode.textContent || '').trim() : '',
			rating:  match ? parseInt(match[1], 10) : 0,
			date:    dateNode ? (dateNode.textContent || '').trim() : '',
			text:    textNode ? (textNode.textContent || '').trim() : '',
			pic_url: imgNode  ? (imgNode.getAttribute('src') || '').trim() : '',
		};
	}).filter(r => {
		if (!r.name || !r.date || r.rating === 0) return false;
		const key = r.name + '|' + r.date;
		if (seen.has(key)) return false;
		seen.add(key);
		return true;
	});
})()
`

// placeJS extracts the raw place JSON string from window.APP_INITIALIZATION_STATE.
const placeJS = `
(function() {
	if (!window.APP_INITIALIZATION_STATE || !window.APP_INITIALIZATION_STATE[3]) {
		return null;
	}
	const appState = window.APP_INITIALIZATION_STATE[3];

	// Search all properties of appState for arrays containing JSON strings
	for (const key of Object.keys(appState)) {
		const arr = appState[key];
		if (Array.isArray(arr)) {
			// Check indices 6 and 5 (where place data typically is)
			for (const idx of [6, 5]) {
				const item = arr[idx];
				if (typeof item === 'string' && item.startsWith(")]}'")) {
					return item;
				}
			}
		}
	}
	return null;
})()
`
