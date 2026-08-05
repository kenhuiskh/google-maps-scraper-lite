package gmaps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log"
	"net/url"
	"regexp"
	"strconv"
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

	// Reviews that have been edited carry a prefix in front of the relative date
	// ("Edited 5 days ago", "上次編輯：5 天前", "已编辑：5 天前"). Stripping it
	// lets the normal unit rules match.
	editedPrefixRE = regexp.MustCompile(`^(?i:edited)\s*|^(?:上次編輯|上次编辑|已編輯|已编辑)\s*[:：]?\s*`)

	// Sub-day units. Output granularity is a day, so these all resolve to today
	// rather than being dropped, which is what happened before they existed —
	// on English UIs as much as Chinese ones.
	subDayAgoRE   = regexp.MustCompile(`(?i)^(?:\d+|an?)\s+(?:second|minute|hour)s?\s+ago$`)
	zhSubDayAgoRE = regexp.MustCompile(`^(?:\d+|[一二兩两三四五六七八九十]+)\s*(?:秒|分鐘|分钟|分|小時|小时)前$`)

	// Chinese relative dates. Review cards are scraped from the DOM, so their
	// timestamps arrive in the UI language: -lang zh-tw yields "3 個月前", not
	// "3 months ago", and every such date parsed to "" before these existed.
	// Traditional and simplified forms are both accepted because the same job
	// can be run with either code. The count is normally an Arabic numeral but
	// "一年前" also appears.
	zhCount      = `(\d+|[一二兩两三四五六七八九十]+)`
	zhTodayRE    = regexp.MustCompile(`^(剛剛|刚刚|現在|现在|今天|今日)$`)
	zhYesterday  = regexp.MustCompile(`^(昨天|昨日)$`)
	zhDaysAgoRE  = regexp.MustCompile(`^` + zhCount + `\s*[天日]前$`)
	zhWeeksAgoRE = regexp.MustCompile(`^` + zhCount + `\s*(?:個|个)?\s*(?:週|周|星期|禮拜|礼拜)前$`)
	// 個月/个月 must be matched as a unit; a bare 月 rule would read the 個 of
	// "3 個月前" as part of the count.
	zhMonthsAgoRE = regexp.MustCompile(`^` + zhCount + `\s*(?:個|个)\s*月前$`)
	zhYearsAgoRE  = regexp.MustCompile(`^` + zhCount + `\s*年前$`)
)

// cjkDigits maps the Chinese numerals that appear in Maps' relative dates. 兩/两
// are the counting form of 2.
var cjkDigits = map[rune]int{
	'一': 1, '二': 2, '兩': 2, '两': 2, '三': 3, '四': 4,
	'五': 5, '六': 6, '七': 7, '八': 8, '九': 9,
}

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

		// The chip bar is normally on the Overview pane and was already read
		// above. When it was not yet rendered there, the reviews panel this
		// stage just opened carries it, so a second read costs one probe and
		// recovers the tags instead of emitting an empty list.
		if len(entry.ReviewTags) == 0 {
			finishStage = tracePlaceStage(ctx, "place.review_tags_retry")
			entry.ReviewTags = extractReviewTags(page)
			finishStage()
		}
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

		// Every selector is DOM-probed before it is clicked, so a click only
		// runs against an element already known to be attached. The attach wait
		// exists for the hydration race between probe and click, not for
		// discovery, which drops the ClickForce hard ceiling from 7s to 3.3s.
		clickWaitTimeout   = 800 * time.Millisecond
		clickActionTimeout = 2000 * time.Millisecond
	)

	// Nothing to page for: Maps already handed us every review it has, or the
	// place has none. The click chain below costs 15-45s per place, so skipping
	// it here is the single largest saving on a mixed corpus.
	if !extraReviewsWorthOpening(entry, targetReviews) {
		return nil
	}

	// Click the reviews tab to open the reviews panel. The tab's data-tab-index
	// is 1 or 2 depending on whether the place also has a Menu tab, so indices
	// are enumerated rather than guessed, and every click is confirmed against
	// review cards actually appearing — a "successful" click on the Menu tab is
	// otherwise indistinguishable from success and ends the chain early.
	reviewSelectors := []string{
		`button[aria-label*="review" i][jsaction]`,
		`button[aria-label*="reviews" i]`,
		`[role="main"] [role="tab"][data-tab-index="1"]`,
		`[role="main"] [role="tab"][data-tab-index="2"]`,
		`[role="main"] [role="tab"][data-tab-index="3"]`,
	}
	baselineCards, _ := countMatches(page, reviewCardSelector)
	panelOpen := func() bool { return reviewsPanelOpen(page, baselineCards) }
	updatePlaceStageDetail(ctx, "place.extra_reviews", fmt.Sprintf("action=open target=%d", targetReviews))
	opened := clickFirstMatching(ctx, page, "open", targetReviews, reviewSelectors, clickWaitTimeout, clickActionTimeout, scrollPauseMs*time.Millisecond, panelOpen)

	// Without an open reviews panel there is no sort control to find, and the
	// scroll loop below has nothing to page. Both chains would be pure latency.
	if opened == "" {
		return nil
	}

	// Sort by "Newest": open the sort menu, then pick the newest option. The sort
	// button has no locale-independent attribute, so it is resolved in JS by its
	// icon glyph and stamped with data-gms-sort; the English attribute selectors
	// remain as fallbacks in case that glyph ever changes.
	sortButtonSelectors := make([]string, 0, 3)
	if resolveSortControl(ctx, page) {
		sortButtonSelectors = append(sortButtonSelectors, sortControlTagSelector)
	}
	sortButtonSelectors = append(sortButtonSelectors, sortControlSelector, `button[aria-label*="Sort" i]`)

	sortMenuOpen := func() bool {
		return page.WaitSelector(`[role="menu"] [role="menuitemradio"], [role="menuitemradio"]`, 1500*time.Millisecond) == nil
	}

	updatePlaceStageDetail(ctx, "place.extra_reviews", fmt.Sprintf("action=open_sort target=%d", targetReviews))
	sorted := clickFirstMatching(ctx, page, "open_sort", targetReviews, sortButtonSelectors, clickWaitTimeout, clickActionTimeout, scrollPauseMs*time.Millisecond, sortMenuOpen)

	if sorted == "" {
		logSortCandidates(ctx, page)
	}

	if sorted != "" {
		newestSelectors := []string{
			`[role="menu"] [role="menuitemradio"][data-index="1"]`,
			`[role="menuitemradio"][data-index="1"]`,
			`[role="menuitemradio"][aria-label*="Newest" i]`,
		}
		updatePlaceStageDetail(ctx, "place.extra_reviews", fmt.Sprintf("action=select_newest target=%d", targetReviews))
		clickFirstMatching(ctx, page, "select_newest", targetReviews, newestSelectors, clickWaitTimeout, clickActionTimeout, scrollPauseMs*time.Millisecond, nil)
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

	log.Printf(
		"DIAG event=extra_reviews_done collected=%d target=%d scrolls=%d stale=%d %s",
		len(entry.UserReviews)+len(entry.UserReviewsExtended), targetReviews,
		scrollCount, staleCount, claimContextDiagnostic(ctx),
	)

	return nil
}

const (
	reviewCardSelector = `[data-review-id]`
	// sortControlSelector matches the reviews-panel sort button on an English UI
	// only: data-value carries the *translated* label, so zh-tw renders
	// data-value="排序". It stays as a fallback behind resolveSortControl.
	sortControlSelector = `button[data-value="Sort"]`
	// sortControlTagSelector matches the button resolveSortControl stamped, so
	// the click still goes through ClickForce as a trusted CDP event rather
	// than an untrusted JS dispatch Maps' handlers would ignore.
	sortControlTagSelector = `[data-gms-sort="1"]`
)

// resolveSortControlJS tags the reviews-panel sort button and returns its label,
// or "" when there is nothing to tag.
//
// Maps renders that button's text as a Material icon ligature in the private use
// area (U+E164) followed by the translated word — "Sort" on en,
// "排序" on zh-tw. The glyph is identical across locales; the word, the
// aria-label, and data-value are all translated, which is why a CSS selector
// alone cannot find this control.
const resolveSortControlJS = `
(function() {
	document.querySelectorAll('[data-gms-sort]').forEach(function(node) {
		node.removeAttribute('data-gms-sort');
	});

	const sortIcon = String.fromCharCode(0xE164);
	const scope = document.querySelector('[role="main"]') || document;
	const buttons = Array.from(scope.querySelectorAll('button, [role="button"]'));
	const match = buttons.find(function(button) {
		return (button.textContent || '').indexOf(sortIcon) !== -1;
	});
	if (!match) return '';

	match.setAttribute('data-gms-sort', '1');
	return (match.getAttribute('aria-label') || match.textContent || 'sort').trim().slice(0, 60);
})()
`

// resolveSortControl tags the sort button in the DOM and reports whether one was
// found, so the caller can skip a selector that is guaranteed to miss.
func resolveSortControl(ctx context.Context, page Page) bool {
	// The panel header renders after its review cards, so a single probe taken
	// right after the open click can miss a control that is merely late.
	const (
		attempts = 4
		pause    = 600 * time.Millisecond
	)
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			page.Sleep(pause)
		}
		result, err := page.Evaluate(resolveSortControlJS)
		if err != nil {
			return false
		}
		label, _ := result.(string)
		if label == "" {
			continue
		}
		log.Printf(
			"DIAG event=sort_control_resolved label=%q stage=place.extra_reviews %s",
			truncateDiagnostic(label, 60),
			claimContextDiagnostic(ctx),
		)
		return true
	}
	return false
}

// reviewsPanelOpen distinguishes an open reviews panel from the place Overview
// pane, which renders a handful of review cards of its own: 46 of 48 places in
// the captured diagnostics already had [data-review-id] attached before any
// click, so card presence alone would confirm every click — including the
// Menu-tab mis-click this check exists to reject.
//
// Growth past the Overview card baseline is the primary signal. The English
// sort-control selector is checked first only because it is a cheap exact hit on
// an en UI; it is expected to miss on every other locale, where data-value
// carries the translated label.
func reviewsPanelOpen(page Page, baselineCards int) bool {
	// The panel hydrates asynchronously after the click settles, so poll a
	// bounded number of times rather than judging on a single sample.
	const (
		attempts = 5
		pause    = 400 * time.Millisecond
	)
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			page.Sleep(pause)
		}
		if count, ok := countMatches(page, sortControlSelector); ok && count > 0 {
			return true
		}
		if count, ok := countMatches(page, reviewCardSelector); ok && count > baselineCards {
			return true
		}
	}
	return false
}

// countMatches returns how many elements match selector. ok is false when the
// probe itself failed, so callers can tell "no matches" from "no answer".
func countMatches(page Page, selector string) (count int, ok bool) {
	result, err := page.Evaluate(clickTargetJS(selector))
	if err != nil {
		return 0, false
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return 0, false
	}
	var probe clickTargetProbe
	if err := json.Unmarshal(raw, &probe); err != nil {
		return 0, false
	}
	return probe.Count, true
}

// extraReviewsWorthOpening reports whether paging the reviews panel can still
// add reviews beyond what EntryFromJSON already parsed out of the page blob.
// ReviewCount is Google's own total, so it bounds what any amount of scrolling
// could ever yield.
func extraReviewsWorthOpening(entry *Entry, targetReviews int) bool {
	have := len(entry.UserReviews) + len(entry.UserReviewsExtended)
	if have >= targetReviews {
		return false
	}
	if entry.ReviewCount <= 0 {
		// A place Maps reports as having no reviews at all. Guard on `have` too,
		// so a review_count that failed to parse never silently skips a place
		// that demonstrably has reviews.
		return have > 0
	}
	return have < entry.ReviewCount
}

// clickFirstMatching tries selectors in order, returning the selector that was
// clicked or "" when none matched. Every error remains a selector miss, and a
// successful click still settles before the helper returns.
//
// A selector the DOM probe reports as matching nothing is skipped without a
// ClickForce: the driver call could only wait out its attach timeout and fail,
// and on a non-English UI most of a chain misses that way.
//
// verify, when non-nil, is the post-click check that the click had the intended
// effect. Maps' tab strip makes a wrong-but-clickable target (the Menu tab where
// the Reviews tab was expected) look identical to success, so an unverified
// click ends the chain on the wrong element; a failed verify falls through to
// the next selector instead.
func clickFirstMatching(ctx context.Context, page Page, action string, targetReviews int, selectors []string, waitTimeout, clickTimeout, settle time.Duration, verify func() bool) string {
	for _, selector := range selectors {
		updatePlaceStageDetail(ctx, "place.extra_reviews", extraReviewsClickDetail(action, targetReviews, "trying", selector))

		if count, probed := logClickTarget(ctx, page, action, selector); probed && count == 0 {
			updatePlaceStageDetail(ctx, "place.extra_reviews", extraReviewsClickDetail(action, targetReviews, "absent", selector))
			continue
		}

		err := page.ClickForce(selector, waitTimeout, clickTimeout)
		if err != nil {
			if errors.Is(err, ErrClickHardTimeout) {
				logClickHardTimeout(ctx, action, selector, err)
			}
			continue
		}

		page.Sleep(settle)

		if verify != nil && !verify() {
			updatePlaceStageDetail(ctx, "place.extra_reviews", extraReviewsClickDetail(action, targetReviews, "unverified", selector))
			logClickUnverified(ctx, action, selector)
			continue
		}

		updatePlaceStageDetail(ctx, "place.extra_reviews", extraReviewsClickDetail(action, targetReviews, "matched", selector))
		return selector
	}
	return ""
}

// sortCandidatesJS lists the reviews panel's own controls. Google changes these
// handles over time and per locale, and the sort control is the one piece of the
// chain with no other way to identify it, so an exhausted sort chain records
// what was actually on the page.
const sortCandidatesJS = `
(function() {
	const nodes = document.querySelectorAll('[role="main"] button, [role="main"] [role="button"]');
	const out = [];
	for (const node of nodes) {
		const data = {};
		for (const attribute of node.attributes) {
			if (attribute.name.startsWith('data-')) {
				data[attribute.name] = attribute.value;
			}
		}
		if (Object.keys(data).length === 0) continue;
		out.push({
			text: (node.textContent || '').trim().slice(0, 24),
			ariaLabel: (node.getAttribute('aria-label') || '').slice(0, 60),
			data: data,
		});
	}
	return JSON.stringify(out).slice(0, 1800);
})()
`

func logSortCandidates(ctx context.Context, page Page) {
	result, err := page.Evaluate(sortCandidatesJS)
	if err != nil {
		return
	}
	dump, _ := result.(string)
	if dump == "" {
		return
	}
	log.Printf(
		"DIAG event=sort_candidates stage=place.extra_reviews candidates=%q %s",
		truncateDiagnostic(dump, 1800),
		claimContextDiagnostic(ctx),
	)
}

func logClickUnverified(ctx context.Context, action, selector string) {
	log.Printf(
		"DIAG event=click_unverified selector=%q action=%s stage=place.extra_reviews %s",
		truncateDiagnostic(selector, 300),
		truncateDiagnostic(action, 80),
		claimContextDiagnostic(ctx),
	)
}

func extraReviewsClickDetail(action string, targetReviews int, state, selector string) string {
	return fmt.Sprintf("action=%s %s=%s target=%d", action, state, truncateDiagnostic(selector, 120), targetReviews)
}

func logClickHardTimeout(ctx context.Context, action, selector string, err error) {
	ceiling := "unknown"
	var timeoutErr *ClickHardTimeoutError
	if errors.As(err, &timeoutErr) && timeoutErr != nil {
		ceiling = timeoutErr.Ceiling.String()
	}
	log.Printf(
		"DIAG event=click_hard_timeout selector=%q ceiling=%s action=%s stage=place.extra_reviews %s",
		truncateDiagnostic(selector, 300),
		ceiling,
		truncateDiagnostic(action, 80),
		claimContextDiagnostic(ctx),
	)
}

type clickTargetGeometry struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

type clickTargetProbe struct {
	Count            int                 `json:"count"`
	TagName          string              `json:"tagName"`
	Role             string              `json:"role"`
	AriaLabel        string              `json:"ariaLabel"`
	DataAttributes   map[string]string   `json:"dataAttributes"`
	TextContent      string              `json:"textContent"`
	Geometry         clickTargetGeometry `json:"geometry"`
	OffsetParentNull bool                `json:"offsetParentNull"`
	Visibility       string              `json:"visibility"`
	PointerEvents    string              `json:"pointerEvents"`
	RefineReviews    bool                `json:"refineReviews"`
	ReviewCards      bool                `json:"reviewCards"`
	Error            string              `json:"error"`
}

// logClickTarget probes selector and emits the click_target diagnostic. It
// returns the match count and whether the probe itself succeeded; callers use
// the pair to skip clicks that cannot match. A failed probe reports probed=false
// so the caller still attempts the click rather than trusting a zero it never
// measured.
func logClickTarget(ctx context.Context, page Page, action, selector string) (count int, probed bool) {
	result, err := page.Evaluate(clickTargetJS(selector))
	if err != nil {
		logClickTargetError(ctx, action, selector, err)
		return 0, false
	}

	raw, err := json.Marshal(result)
	if err != nil {
		logClickTargetError(ctx, action, selector, err)
		return 0, false
	}

	var target clickTargetProbe
	if err := json.Unmarshal(raw, &target); err != nil {
		logClickTargetError(ctx, action, selector, err)
		return 0, false
	}

	dataAttributes := "{}"
	if target.DataAttributes != nil {
		if data, err := json.Marshal(target.DataAttributes); err == nil {
			dataAttributes = string(data)
		}
	}
	geometry := "{}"
	if data, err := json.Marshal(target.Geometry); err == nil {
		geometry = string(data)
	}

	log.Printf(
		"DIAG event=click_target action=%s selector=%q count=%d tag_name=%q role=%q aria_label=%q data_attributes=%q text_content=%q geometry=%q offset_parent_null=%t visibility=%q pointer_events=%q refine_reviews=%t review_cards=%t error=%q %s",
		truncateDiagnostic(action, 80),
		truncateDiagnostic(selector, 300),
		target.Count,
		truncateDiagnostic(target.TagName, 50),
		truncateDiagnostic(target.Role, 100),
		truncateDiagnostic(target.AriaLabel, 300),
		truncateDiagnostic(dataAttributes, 500),
		truncateDiagnostic(target.TextContent, 100),
		truncateDiagnostic(geometry, 200),
		target.OffsetParentNull,
		truncateDiagnostic(target.Visibility, 100),
		truncateDiagnostic(target.PointerEvents, 100),
		target.RefineReviews,
		target.ReviewCards,
		truncateDiagnostic(target.Error, 300),
		claimContextDiagnostic(ctx),
	)

	return target.Count, true
}

func logClickTargetError(ctx context.Context, action, selector string, err error) {
	log.Printf(
		"DIAG event=click_target action=%s selector=%q error=%q %s",
		truncateDiagnostic(action, 80),
		truncateDiagnostic(selector, 300),
		truncateDiagnostic(err.Error(), 300),
		claimContextDiagnostic(ctx),
	)
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
	var unparsedDates []string
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
		if review.When == "" && strings.TrimSpace(r.Date) != "" {
			unparsedDates = append(unparsedDates, strings.TrimSpace(r.Date))
		}
		reviews = append(reviews, review)
	}
	logUnparsedReviewDates(unparsedDates)

	return reviews, nil
}

// logUnparsedReviewDates records the distinct relative-date strings that
// relativeToAbsoluteDate could not read. Review dates arrive in the UI language,
// so this is how a new locale's wording surfaces instead of silently landing as
// an empty When.
func logUnparsedReviewDates(dates []string) {
	if len(dates) == 0 {
		return
	}

	const maxReported = 5
	seen := make(map[string]bool, len(dates))
	distinct := make([]string, 0, maxReported)
	for _, date := range dates {
		if seen[date] {
			continue
		}
		seen[date] = true
		if len(distinct) < maxReported {
			distinct = append(distinct, date)
		}
	}

	log.Printf(
		"DIAG event=review_date_unparsed count=%d distinct=%d samples=%q",
		len(dates), len(seen), truncateDiagnostic(strings.Join(distinct, " | "), 200),
	)
}

func relativeToAbsoluteDate(s string) string {
	s = strings.TrimSpace(editedPrefixRE.ReplaceAllString(strings.TrimSpace(s), ""))
	if s == "" {
		return ""
	}

	now := time.Now()
	ago := func(years, months, days int) string {
		t := now.AddDate(years, months, days)
		return fmt.Sprintf("%d-%d-%d", t.Year(), int(t.Month()), t.Day())
	}

	switch {
	case todayAgoRE.MatchString(s), zhTodayRE.MatchString(s),
		subDayAgoRE.MatchString(s), zhSubDayAgoRE.MatchString(s):
		return ago(0, 0, 0)
	case zhYesterday.MatchString(s):
		return ago(0, 0, -1)
	case aWeekAgoRE.MatchString(s):
		return ago(0, 0, -7)
	case aMonthAgoRE.MatchString(s):
		return ago(0, -1, 0)
	case aYearAgoRE.MatchString(s):
		return ago(-1, 0, 0)
	}

	// Each entry is the offset for a count of one, scaled by the parsed count.
	for _, unit := range []struct {
		re                  *regexp.Regexp
		years, months, days int
	}{
		{daysAgoRE, 0, 0, -1},
		{zhDaysAgoRE, 0, 0, -1},
		{weeksAgoRE, 0, 0, -7},
		{zhWeeksAgoRE, 0, 0, -7},
		{monthsAgoRE, 0, -1, 0},
		{zhMonthsAgoRE, 0, -1, 0},
		{yearsAgoRE, -1, 0, 0},
		{zhYearsAgoRE, -1, 0, 0},
	} {
		matches := unit.re.FindStringSubmatch(s)
		if len(matches) != 2 {
			continue
		}
		count := parseRelativeCount(matches[1])
		if count <= 0 {
			continue
		}
		return ago(unit.years*count, unit.months*count, unit.days*count)
	}

	return ""
}

// parseRelativeCount reads the quantity out of a relative date, accepting both
// Arabic numerals and the Chinese numerals up to 99 that Maps occasionally uses
// ("一年前"). It returns 0 when the text is not a count.
func parseRelativeCount(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}

	if count, err := strconv.Atoi(s); err == nil {
		return count
	}

	// Chinese numerals are positional around 十: 十=10, 十五=15, 五十=50,
	// 五十三=53. Anything else is left to the caller as "not a count".
	runes := []rune(s)
	tensIdx := -1
	for i, r := range runes {
		if r == '十' {
			tensIdx = i
			break
		}
	}
	if tensIdx < 0 {
		if len(runes) != 1 {
			return 0
		}
		return cjkDigits[runes[0]]
	}

	tens := 1
	if tensIdx > 0 {
		if tensIdx != 1 {
			return 0
		}
		if tens = cjkDigits[runes[0]]; tens == 0 {
			return 0
		}
	}

	ones := 0
	if rest := runes[tensIdx+1:]; len(rest) > 0 {
		if len(rest) != 1 {
			return 0
		}
		if ones = cjkDigits[rest[0]]; ones == 0 {
			return 0
		}
	}

	return tens*10 + ones
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
	if err := page.WaitSelector(reviewTagChipSelector, 500*time.Millisecond); err != nil {
		logReviewTagCandidates(page)
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

// reviewTagChipSelector matches a real review-topic chip in any UI language.
const reviewTagChipSelector = `button[role="radio"][data-index]`

const reviewTagCandidatesJS = `
(function() {
	const chips = Array.from(document.querySelectorAll('button[role="radio"]'));
	if (chips.length === 0) return '';
	const parent = chips[0].parentElement;
	const grandparent = parent ? parent.parentElement : null;
	return JSON.stringify({
		chipCount: chips.length,
		parentLabel: parent ? (parent.getAttribute('aria-label') || '') : '',
		grandparentLabel: grandparent ? (grandparent.getAttribute('aria-label') || '') : '',
		chips: chips.slice(0, 4).map(function(chip) {
			const data = {};
			for (const attribute of chip.attributes) {
				if (attribute.name.startsWith('data-')) data[attribute.name] = attribute.value;
			}
			return {
				label: (chip.getAttribute('aria-label') || '').slice(0, 50),
				text: (chip.textContent || '').trim().slice(0, 30),
				spans: Array.from(chip.querySelectorAll('span')).slice(0, 4).map(function(span) {
					return { cls: span.className, text: (span.textContent || '').trim().slice(0, 20) };
				}),
				data: data,
			};
		}),
	}).slice(0, 1500);
})()
`

func logReviewTagCandidates(page Page) {
	result, err := page.Evaluate(reviewTagCandidatesJS)
	if err != nil {
		return
	}
	dump, _ := result.(string)
	if dump == "" {
		return
	}
	log.Printf("DIAG event=review_tag_candidates dump=%q", truncateDiagnostic(dump, 1500))
}

// reviewTagsJS extracts review keyword chips from the review-topics bar.
// Tag name comes from span.uEubGf. Count is extracted from span.bC3Nkc first;
// if that class is absent (Google Maps layout changes), falls back to finding
// any span inside the button whose text is a pure integer and differs from the tag name.
//
// Chips are selected by data-index rather than through a container matched on
// aria-label, and the "All reviews" / "View N more" chips are excluded
// structurally: every label on this bar is translated ("篩選評論",
// "所有評論"), so the previous English string comparisons returned no tags at
// all on any non-English UI. Only real topic chips carry data-index; the "All"
// chip has none, and "View N more" renders its name as "+N".
const reviewTagsJS = `
(function() {
	const buttons = document.querySelectorAll('button[role="radio"][data-index]');
	const tags = [];

	for (const btn of buttons) {
		const nameSpan = btn.querySelector('span.uEubGf');
		const name = nameSpan ? nameSpan.textContent.trim() : '';
		if (!name || /^\+\d+$/.test(name)) continue;

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

// clickTargetJS JSON-encodes the selector before embedding it in the probe so
// quotes, backslashes, and malformed-selector test cases cannot alter the JS.
const clickTargetProbeJS = `
(function(selector) {
	const result = {
		count: 0,
		refineReviews: !!document.querySelector('[aria-label="Refine reviews"]'),
		reviewCards: !!document.querySelector('[data-review-id]'),
	};

	try {
		const matches = document.querySelectorAll(selector);
		result.count = matches.length;
		const first = matches[0];
		if (!first) return result;

		const rect = first.getBoundingClientRect();
		const dataAttributes = {};
		for (const attribute of first.attributes) {
			if (attribute.name.startsWith('data-')) {
				dataAttributes[attribute.name] = attribute.value;
			}
		}

		const style = getComputedStyle(first);
		result.tagName = first.tagName || '';
		result.role = first.getAttribute('role') || '';
		result.ariaLabel = first.getAttribute('aria-label') || '';
		result.dataAttributes = dataAttributes;
		result.textContent = (first.textContent || '').trim().slice(0, 40);
		result.geometry = { x: rect.x, y: rect.y, w: rect.width, h: rect.height };
		result.offsetParentNull = first.offsetParent === null;
		result.visibility = style.visibility;
		result.pointerEvents = style.pointerEvents;
	} catch (err) {
		result.error = String(err);
	}

	return result;
})(`

func clickTargetJS(selector string) string {
	encoded, _ := json.Marshal(selector)
	return clickTargetProbeJS + string(encoded) + `)`
}

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
