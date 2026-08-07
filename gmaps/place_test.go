package gmaps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

type reviewTagsTestPage struct {
	waitSelector string
	waitTimeout  time.Duration
	waitErr      error
	evaluate     any
}

func (p *reviewTagsTestPage) Goto(string) (int, error)     { return 200, nil }
func (p *reviewTagsTestPage) Reload() (int, error)         { return 200, nil }
func (p *reviewTagsTestPage) Content() (string, error)     { return "", nil }
func (p *reviewTagsTestPage) Evaluate(string) (any, error) { return p.evaluate, nil }
func (p *reviewTagsTestPage) ClickForce(string, time.Duration, time.Duration) error {
	return nil
}
func (p *reviewTagsTestPage) URL() string         { return "" }
func (p *reviewTagsTestPage) Sleep(time.Duration) {}
func (p *reviewTagsTestPage) Close() error        { return nil }
func (p *reviewTagsTestPage) IsClosed() bool      { return false }
func (p *reviewTagsTestPage) WaitSelector(selector string, timeout time.Duration) error {
	p.waitSelector = selector
	p.waitTimeout = timeout
	return p.waitErr
}

func TestExtractReviewTagsUsesShortHydrationGrace(t *testing.T) {
	page := &reviewTagsTestPage{waitErr: errors.New("not found")}
	if got := extractReviewTags(page); len(got) != 0 {
		t.Fatalf("extractReviewTags() = %#v, want empty", got)
	}
	if page.waitSelector != reviewTagChipSelector {
		t.Fatalf("selector = %q", page.waitSelector)
	}
	if page.waitTimeout != 500*time.Millisecond {
		t.Fatalf("timeout = %s, want 500ms", page.waitTimeout)
	}
}

func TestExtractReviewTagsParsesAttachedChips(t *testing.T) {
	page := &reviewTagsTestPage{evaluate: []any{
		map[string]any{"tag": "coffee", "count": float64(12)},
	}}
	got := extractReviewTags(page)
	if len(got) != 1 || got[0].Tag != "coffee" || got[0].Count == nil || *got[0].Count != 12 {
		t.Fatalf("extractReviewTags() = %#v", got)
	}
}

func TestPlaceURLWithLangDoesNotDuplicateExistingHL(t *testing.T) {
	got := placeURLWithLang("https://www.google.com/maps/place/Test?authuser=0&hl=en&rclk=1", "en")
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}

	q := parsed.Query()
	if !reflect.DeepEqual(q["hl"], []string{"en"}) {
		t.Fatalf("hl query values = %#v, want one en", q["hl"])
	}
	if q.Get("authuser") != "0" || q.Get("rclk") != "1" {
		t.Fatalf("query values = %v, want existing parameters preserved", q)
	}
}

func TestPlaceURLWithLangReplacesExistingHL(t *testing.T) {
	got := placeURLWithLang("https://www.google.com/maps/place/Test?authuser=0&hl=fr&rclk=1", "en")
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}

	if got := parsed.Query()["hl"]; !reflect.DeepEqual(got, []string{"en"}) {
		t.Fatalf("hl query values = %#v, want one en", got)
	}
}

func TestPlaceURLWithLangAddsHL(t *testing.T) {
	tests := []string{
		"https://www.google.com/maps/place/Test",
		"https://www.google.com/maps/place/Test?authuser=0",
	}

	for _, tt := range tests {
		got := placeURLWithLang(tt, "en")
		parsed, err := url.Parse(got)
		if err != nil {
			t.Fatalf("parse URL %q: %v", got, err)
		}
		if parsed.Query().Get("hl") != "en" {
			t.Fatalf("placeURLWithLang(%q, en) = %q, want hl=en", tt, got)
		}
	}
}

func TestPlaceIDToURLUsesOfficialMapsURL(t *testing.T) {
	got := PlaceIDToURL("ChIJ123")
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}
	if parsed.Scheme != "https" || parsed.Host != "www.google.com" || parsed.Path != "/maps/search/" {
		t.Fatalf("url = %q, want Google Maps search URL", got)
	}
	q := parsed.Query()
	if q.Get("api") != "1" {
		t.Fatalf("api = %q, want 1", q.Get("api"))
	}
	if q.Get("query") != "place_id:ChIJ123" {
		t.Fatalf("query = %q, want place_id:ChIJ123", q.Get("query"))
	}
	if q.Get("query_place_id") != "ChIJ123" {
		t.Fatalf("query_place_id = %q, want ChIJ123", q.Get("query_place_id"))
	}
}

func TestChoosePlaceLinkPrefersCanonicalBrowserURL(t *testing.T) {
	canonical := "https://www.google.com/maps/place/Test/data=!4m2!3m1!1s0x1:0x2"
	fallback := PlaceIDToURL("ChIJ123")
	got := choosePlaceLink("https://www.google.com/maps/place/Old/data=!4m2", canonical, fallback)
	if got != canonical {
		t.Fatalf("link = %q, want canonical %q", got, canonical)
	}
}

func TestChoosePlaceLinkKeepsGoodParsedLinkOverPlaceIDFallback(t *testing.T) {
	parsed := "https://www.google.com/maps/place/Test/data=!4m2!3m1!1s0x1:0x2"
	fallback := PlaceIDToURL("ChIJ123")
	got := choosePlaceLink(parsed, fallback, fallback)
	if got != parsed {
		t.Fatalf("link = %q, want parsed %q", got, parsed)
	}
}

func TestChoosePlaceLinkFallsBackWhenNoCanonical(t *testing.T) {
	fallback := PlaceIDToURL("ChIJ123")
	got := choosePlaceLink("", "", fallback)
	if got != fallback {
		t.Fatalf("link = %q, want fallback %q", got, fallback)
	}
}

func TestScrapeExtraReviewsUsesQualifiedFallbackSelectors(t *testing.T) {
	// Every open-chain click fails, so the chain is exhausted and the sort chain
	// is never reached: without an open reviews panel there is nothing to sort.
	want := []string{
		`button[aria-label*="review" i][jsaction]`,
		`button[aria-label*="reviews" i]`,
		`[role="main"] [role="tab"][data-tab-index="1"]`,
		`[role="main"] [role="tab"][data-tab-index="2"]`,
	}
	clickErrors := make(map[string]error, len(want))
	for _, selector := range want {
		clickErrors[selector] = errors.New("selector miss")
	}
	page := &clickFirstMatchingTestPage{clickErrors: clickErrors, waitSelectorErr: errors.New("no review cards")}

	_ = scrapeExtraReviews(context.Background(), page, &Entry{ReviewCount: 50}, 10)

	if !reflect.DeepEqual(page.clicked, want) {
		t.Fatalf("fallback selectors passed to ClickForce = %#v, want %#v", page.clicked, want)
	}
	for _, selector := range page.clicked {
		if selector == `[data-tab-index="1"]` || selector == `[data-index="1"]` || selector == `[data-value="2"]` {
			t.Fatalf("bare fallback selector passed to ClickForce: %q", selector)
		}
	}
}

func TestScrapeExtraReviewsUsesQualifiedSortSelectors(t *testing.T) {
	// No sort label resolves, so only the English attribute fallbacks are tried.
	want := []string{
		`button[data-value="Sort"]`,
		`button[aria-label*="Sort" i]`,
	}
	clickErrors := make(map[string]error, len(want))
	for _, selector := range want {
		clickErrors[selector] = errors.New("selector miss")
	}
	// WaitSelector succeeds, so the first open selector verifies and the chain
	// moves on to the sort controls.
	page := &clickFirstMatchingTestPage{clickErrors: clickErrors}

	_ = scrapeExtraReviews(context.Background(), page, &Entry{ReviewCount: 50}, 10)

	if len(page.clicked) < 1+len(want) {
		t.Fatalf("clicked selectors = %#v, want an open click followed by %#v", page.clicked, want)
	}
	if got := page.clicked[1 : 1+len(want)]; !reflect.DeepEqual(got, want) {
		t.Fatalf("sort selectors passed to ClickForce = %#v, want %#v", got, want)
	}
}

func TestScrapeExtraReviewsSkipsWhenNoReviewsRemain(t *testing.T) {
	for name, entry := range map[string]*Entry{
		"no reviews at all":  {ReviewCount: 0},
		"target already met": {ReviewCount: 500, UserReviews: make([]Review, 10)},
		"all reviews parsed": {ReviewCount: 3, UserReviews: make([]Review, 3)},
	} {
		t.Run(name, func(t *testing.T) {
			page := &clickFirstMatchingTestPage{}
			if err := scrapeExtraReviews(context.Background(), page, entry, 10); err != nil {
				t.Fatalf("scrapeExtraReviews() error: %v", err)
			}
			if len(page.clicked) != 0 {
				t.Fatalf("clicked selectors = %#v, want no clicks", page.clicked)
			}
		})
	}
}

// The Overview pane already renders review cards, so a click that reveals no
// sort control and no additional cards has not opened the reviews panel — the
// Menu-tab mis-click that ended the chain early in the captured diagnostics.
func TestReviewsPanelOpenRejectsUnchangedOverviewPane(t *testing.T) {
	page := &clickFirstMatchingTestPage{probeCounts: map[string]int{
		sortControlSelector: 0,
		reviewCardSelector:  3,
	}}

	if reviewsPanelOpen(page, 3) {
		t.Fatal("reviewsPanelOpen() = true for an unchanged Overview pane, want false")
	}
}

func TestReviewsPanelOpenAcceptsSortControlAndCardGrowth(t *testing.T) {
	sortPresent := &clickFirstMatchingTestPage{probeCounts: map[string]int{
		sortControlSelector: 1,
		reviewCardSelector:  3,
	}}
	if !reviewsPanelOpen(sortPresent, 3) {
		t.Fatal("reviewsPanelOpen() = false with a sort control present, want true")
	}

	// Places with too few reviews for Maps to offer sorting still open a panel.
	cardsGrew := &clickFirstMatchingTestPage{probeCounts: map[string]int{
		sortControlSelector: 0,
		reviewCardSelector:  9,
	}}
	if !reviewsPanelOpen(cardsGrew, 3) {
		t.Fatal("reviewsPanelOpen() = false after card growth, want true")
	}
}

func TestClickFirstMatchingSkipsSelectorsThatMatchNothing(t *testing.T) {
	page := &clickFirstMatchingTestPage{probeCounts: map[string]int{"first": 0, "second": 0}}

	got := clickFirstMatching(context.Background(), page, "open", 5, []string{"first", "second", "third"}, time.Millisecond, time.Millisecond, 0, nil)
	if got != "third" {
		t.Fatalf("clickFirstMatching() = %q, want third", got)
	}
	if !reflect.DeepEqual(page.clicked, []string{"third"}) {
		t.Fatalf("clicked selectors = %#v, want only third", page.clicked)
	}
}

func TestClickFirstMatchingRejectsUnverifiedClick(t *testing.T) {
	page := &clickFirstMatchingTestPage{}
	verified := map[string]bool{"third": true}
	var checked []string

	got := clickFirstMatching(context.Background(), page, "open", 5, []string{"first", "second", "third"}, time.Millisecond, time.Millisecond, 0, func() bool {
		selector := page.clicked[len(page.clicked)-1]
		checked = append(checked, selector)
		return verified[selector]
	})
	if got != "third" {
		t.Fatalf("clickFirstMatching() = %q, want third", got)
	}
	if !reflect.DeepEqual(checked, []string{"first", "second", "third"}) {
		t.Fatalf("verified selectors = %#v, want all three", checked)
	}
}

type clickFirstMatchingTestPage struct {
	clickErrors     map[string]error
	probeCounts     map[string]int
	evaluateErr     error
	evaluateCalls   int
	waitSelectorErr error
	clicked         []string
	sleeps          []time.Duration
	detailAtClick   []string
	onClick         func(string)
	probedSelector  string
	sortLabel       string
}

func (p *clickFirstMatchingTestPage) Goto(string) (int, error) { return 200, nil }
func (p *clickFirstMatchingTestPage) Reload() (int, error)     { return 200, nil }
func (p *clickFirstMatchingTestPage) Content() (string, error) { return "", nil }
func (p *clickFirstMatchingTestPage) URL() string              { return "" }
func (p *clickFirstMatchingTestPage) Close() error             { return nil }
func (p *clickFirstMatchingTestPage) IsClosed() bool           { return false }
func (p *clickFirstMatchingTestPage) WaitSelector(string, time.Duration) error {
	return p.waitSelectorErr
}
func (p *clickFirstMatchingTestPage) Sleep(d time.Duration) { p.sleeps = append(p.sleeps, d) }
func (p *clickFirstMatchingTestPage) Evaluate(js string) (any, error) {
	p.evaluateCalls++
	if p.evaluateErr != nil {
		return nil, p.evaluateErr
	}
	if strings.Contains(js, "data-gms-sort") {
		return p.sortLabel, nil
	}
	// The probe embeds its selector as a JSON string literal; recover it so the
	// fake can answer per-selector match counts.
	count := 1
	for selector, want := range p.probeCounts {
		if encoded, err := json.Marshal(selector); err == nil && strings.Contains(js, string(encoded)) {
			p.probedSelector = selector
			count = want
			break
		}
	}
	return map[string]any{"count": float64(count)}, nil
}
func (p *clickFirstMatchingTestPage) ClickForce(selector string, _, _ time.Duration) error {
	p.clicked = append(p.clicked, selector)
	if p.onClick != nil {
		p.onClick(selector)
	}
	if err, ok := p.clickErrors[selector]; ok {
		return err
	}
	return nil
}

func TestClickFirstMatchingAttributesSelectorsAndStageDetails(t *testing.T) {
	trace := &claimTrace{worker: 2, urlID: 17, attempt: 3, lang: "en", url: "https://example.test/place", started: time.Now()}
	ctx := withPlaceTrace(context.Background(), trace)
	updatePlaceStageDetail(ctx, "place.extra_reviews", "action=open target=5")

	page := &clickFirstMatchingTestPage{
		clickErrors: map[string]error{
			"first":  errors.New("not attached"),
			"second": errors.New("not clickable"),
		},
	}
	page.onClick = func(string) {
		page.detailAtClick = append(page.detailAtClick, trace.snapshot().Detail)
	}
	got := clickFirstMatching(ctx, page, "open", 5, []string{"first", "second", "third"}, time.Millisecond, time.Millisecond, 0, nil)
	if got != "third" {
		t.Fatalf("clickFirstMatching() = %q, want third", got)
	}
	if len(page.clicked) != 3 || page.clicked[0] != "first" || page.clicked[2] != "third" {
		t.Fatalf("clicked selectors = %#v", page.clicked)
	}
	if len(page.detailAtClick) != 3 || !strings.Contains(page.detailAtClick[0], "trying=first") || !strings.Contains(page.detailAtClick[1], "trying=second") {
		t.Fatalf("trying details = %#v", page.detailAtClick)
	}
	if detail := trace.snapshot().Detail; !strings.Contains(detail, "matched=third") || !strings.Contains(detail, "target=5") {
		t.Fatalf("final stage detail = %q, want matched selector and target", detail)
	}
	if len(page.sleeps) != 1 || page.sleeps[0] != 0 {
		t.Fatalf("settle sleeps = %#v, want one zero-duration settle", page.sleeps)
	}
}

func TestClickFirstMatchingContinuesWhenClickTargetProbeFails(t *testing.T) {
	logs := captureLog(t)
	page := &clickFirstMatchingTestPage{
		evaluateErr: errors.New("probe unavailable"),
		clickErrors: map[string]error{
			"first": errors.New("not attached"),
		},
	}
	got := clickFirstMatching(context.Background(), page, "open", 5, []string{"first", "second"}, time.Millisecond, time.Millisecond, 0, nil)
	if got != "second" {
		t.Fatalf("clickFirstMatching() = %q, want second", got)
	}
	if page.evaluateCalls != 2 || len(page.clicked) != 2 {
		t.Fatalf("probe/click calls = %d/%d, want 2/2", page.evaluateCalls, len(page.clicked))
	}
	if !strings.Contains(logs.String(), "DIAG event=click_target") {
		t.Fatalf("expected probe diagnostics after evaluate errors, got %q", logs.String())
	}
}

func TestClickFirstMatchingEmitsHardTimeoutDiagnostic(t *testing.T) {
	logs := captureLog(t)
	trace := &claimTrace{worker: 4, urlID: 31, attempt: 2, lang: "zh-tw", url: "https://example.test/place", started: time.Now()}
	ctx := withPlaceTrace(context.Background(), trace)
	updatePlaceStageDetail(ctx, "place.extra_reviews", "action=open target=5")
	page := &clickFirstMatchingTestPage{
		clickErrors: map[string]error{
			"review-tab": &ClickHardTimeoutError{Selector: "review-tab", Ceiling: 7 * time.Second},
		},
		evaluateErr: errors.New("probe unavailable"),
	}

	if got := clickFirstMatching(ctx, page, "open", 5, []string{"review-tab"}, time.Millisecond, time.Millisecond, 0, nil); got != "" {
		t.Fatalf("clickFirstMatching() = %q, want no match", got)
	}
	logText := logs.String()
	for _, want := range []string{
		"DIAG event=click_hard_timeout",
		`selector="review-tab"`,
		"ceiling=7s",
		"action=open",
		"stage=place.extra_reviews",
		"worker=4 url_id=31 attempt=2 lang=\"zh-tw\"",
	} {
		if !strings.Contains(logText, want) {
			t.Fatalf("hard-timeout diagnostic missing %q:\n%s", want, logText)
		}
	}
}

func TestRelativeToAbsoluteDateParsesChineseReviewDates(t *testing.T) {
	now := time.Now()
	fmtDate := func(t time.Time) string {
		return fmt.Sprintf("%d-%d-%d", t.Year(), int(t.Month()), t.Day())
	}

	tests := map[string]string{
		// Traditional (zh-TW) and simplified (zh-CN) both reach this parser
		// because either code can be passed to -lang.
		"今天":    fmtDate(now),
		"剛剛":    fmtDate(now),
		"昨天":    fmtDate(now.AddDate(0, 0, -1)),
		"3 天前":  fmtDate(now.AddDate(0, 0, -3)),
		"3天前":   fmtDate(now.AddDate(0, 0, -3)),
		"2 週前":  fmtDate(now.AddDate(0, 0, -14)),
		"2 星期前": fmtDate(now.AddDate(0, 0, -14)),
		"2 周前":  fmtDate(now.AddDate(0, 0, -14)),
		"5 個月前": fmtDate(now.AddDate(0, -5, 0)),
		"5 个月前": fmtDate(now.AddDate(0, -5, 0)),
		"1 年前":  fmtDate(now.AddDate(-1, 0, 0)),
		"一年前":   fmtDate(now.AddDate(-1, 0, 0)),
		"兩個月前":  fmtDate(now.AddDate(0, -2, 0)),
		"十天前":   fmtDate(now.AddDate(0, 0, -10)),
		// Sub-day units collapse to today; the output granularity is a day.
		"1 小時前":         fmtDate(now),
		"30 分鐘前":        fmtDate(now),
		"an hour ago":   fmtDate(now),
		"5 minutes ago": fmtDate(now),
		// Edited reviews prefix the relative date.
		"上次編輯：5 天前":           fmtDate(now.AddDate(0, 0, -5)),
		"Edited 2 months ago": fmtDate(now.AddDate(0, -2, 0)),
		// English must keep working unchanged.
		"3 months ago": fmtDate(now.AddDate(0, -3, 0)),
		"a year ago":   fmtDate(now.AddDate(-1, 0, 0)),
	}

	for in, want := range tests {
		if got := relativeToAbsoluteDate(in); got != want {
			t.Errorf("relativeToAbsoluteDate(%q) = %q, want %q", in, got, want)
		}
	}

	for _, in := range []string{"", "not a date", "2025-12-08", "個月前"} {
		if got := relativeToAbsoluteDate(in); got != "" {
			t.Errorf("relativeToAbsoluteDate(%q) = %q, want empty", in, got)
		}
	}
}

func TestParseRelativeCount(t *testing.T) {
	tests := map[string]int{
		"7": 7, "12": 12, "一": 1, "兩": 2, "两": 2, "九": 9,
		"十": 10, "十五": 15, "五十": 50, "五十三": 53,
		"": 0, "abc": 0, "一二三": 0, "十十": 0,
	}
	for in, want := range tests {
		if got := parseRelativeCount(in); got != want {
			t.Errorf("parseRelativeCount(%q) = %d, want %d", in, got, want)
		}
	}
}

// data-value carries the translated label ("排序" on zh-tw), so the sort button
// is resolved in JS and stamped instead; the tagged selector must lead.
func TestScrapeExtraReviewsPrefersResolvedSortControl(t *testing.T) {
	page := &clickFirstMatchingTestPage{
		sortLabel:   "排序評論",
		clickErrors: map[string]error{sortControlTagSelector: errors.New("selector miss")},
	}

	_ = scrapeExtraReviews(context.Background(), page, &Entry{ReviewCount: 50}, 10)

	if len(page.clicked) < 2 {
		t.Fatalf("clicked selectors = %#v, want an open click then a sort click", page.clicked)
	}
	if page.clicked[1] != sortControlTagSelector {
		t.Fatalf("first sort selector = %q, want %q", page.clicked[1], sortControlTagSelector)
	}
}

func TestResolveSortControlReportsMissingControl(t *testing.T) {
	if resolveSortControl(context.Background(), &clickFirstMatchingTestPage{sortLabel: ""}) {
		t.Fatal("resolveSortControl() = true with no control on the page, want false")
	}
	if !resolveSortControl(context.Background(), &clickFirstMatchingTestPage{sortLabel: "Sort reviews"}) {
		t.Fatal("resolveSortControl() = false with a control present, want true")
	}
}

// botCheckTestPage records whether the bot check reached for the page's full
// HTML. Content() is the expensive path this probe exists to avoid, so the tests
// assert it is never called rather than only checking the verdict.
type botCheckTestPage struct {
	url          string
	signals      any
	evaluateErr  error
	contentCalls int
	observed     []string
	observedSize int
	observedName string
}

func (p *botCheckTestPage) Goto(string) (int, error) { return 200, nil }
func (p *botCheckTestPage) Reload() (int, error)     { return 200, nil }
func (p *botCheckTestPage) Content() (string, error) {
	p.contentCalls++
	return "", nil
}

func (p *botCheckTestPage) Evaluate(string) (any, error) {
	if p.evaluateErr != nil {
		return nil, p.evaluateErr
	}
	return p.signals, nil
}
func (p *botCheckTestPage) ClickForce(string, time.Duration, time.Duration) error { return nil }
func (p *botCheckTestPage) URL() string                                           { return p.url }
func (p *botCheckTestPage) Sleep(time.Duration)                                   {}
func (p *botCheckTestPage) Close() error                                          { return nil }
func (p *botCheckTestPage) IsClosed() bool                                        { return false }
func (p *botCheckTestPage) WaitSelector(string, time.Duration) error              { return nil }
func (p *botCheckTestPage) ObservePageDiagnostics(class string, contentBytes int, title string) {
	p.observed = append(p.observed, class)
	p.observedSize = contentBytes
	p.observedName = title
}

var _ PageDiagnosticObserver = (*botCheckTestPage)(nil)

func signalsMap(sig pageSignals) map[string]any {
	return map[string]any{
		"title":   sig.Title,
		"bytes":   float64(sig.Bytes),
		"unusual": sig.Unusual,
		"captcha": sig.Captcha,
		"consent": sig.Consent,
		"maps":    sig.Maps,
	}
}

func TestDetectBotBlockUsesBoundedProbeNotPageContent(t *testing.T) {
	tests := []struct {
		name      string
		url       string
		status    int
		signals   any
		wantBlock bool
		wantClass string
	}{
		{
			name:      "healthy maps page",
			url:       "https://www.google.com/maps/place/Coffee",
			status:    200,
			signals:   signalsMap(pageSignals{Title: "Coffee - Google Maps", Bytes: 2_500_000, Maps: true}),
			wantClass: "maps",
		},
		{
			name:      "unusual traffic",
			url:       "https://www.google.com/maps/place/Coffee",
			status:    200,
			signals:   signalsMap(pageSignals{Title: "Error", Bytes: 4096, Unusual: true}),
			wantBlock: true,
			wantClass: "unusual_traffic",
		},
		{
			name:      "captcha",
			url:       "https://example.test/challenge",
			status:    200,
			signals:   signalsMap(pageSignals{Title: "Verify", Bytes: 8192, Captcha: true}),
			wantClass: "captcha",
		},
		{
			name:      "consent wall",
			url:       "https://example.test/interstitial",
			status:    200,
			signals:   signalsMap(pageSignals{Title: "Before you continue", Bytes: 8192, Consent: true}),
			wantClass: "consent",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			page := &botCheckTestPage{url: tt.url, signals: tt.signals}

			err := detectBotBlock(page, tt.status)

			if gotBlock := errors.Is(err, ErrBotBlocked); gotBlock != tt.wantBlock {
				t.Fatalf("detectBotBlock() blocked = %v (err %v), want %v", gotBlock, err, tt.wantBlock)
			}
			if page.contentCalls != 0 {
				t.Errorf("page.Content() called %d times, want 0", page.contentCalls)
			}
			if len(page.observed) != 1 || page.observed[0] != tt.wantClass {
				t.Errorf("observed classes = %#v, want [%s]", page.observed, tt.wantClass)
			}
		})
	}
}

// The title reaches diagnostics from document.title, so no <title> regex over the
// full HTML is needed; whitespace is still collapsed and entities unescaped.
func TestDetectBotBlockReportsTitleAndSizeFromProbe(t *testing.T) {
	page := &botCheckTestPage{
		url:     "https://www.google.com/maps/place/Coffee",
		signals: signalsMap(pageSignals{Title: "  Ben &amp; Jerry’s\n  - Google Maps ", Bytes: 1_234_567, Maps: true}),
	}

	if err := detectBotBlock(page, 200); err != nil {
		t.Fatalf("detectBotBlock() = %v, want nil", err)
	}
	if want := "Ben & Jerry’s - Google Maps"; page.observedName != want {
		t.Errorf("title = %q, want %q", page.observedName, want)
	}
	if page.observedSize != 1_234_567 {
		t.Errorf("content bytes = %d, want 1234567", page.observedSize)
	}
}

// URL and status alone decide the sorry/consent/429 cases, so those must not pay
// for a probe at all.
func TestDetectBotBlockSkipsProbeForURLAndStatusSignals(t *testing.T) {
	tests := []struct {
		name      string
		url       string
		status    int
		wantClass string
	}{
		{name: "sorry", url: "https://www.google.com/sorry/index", status: 200, wantClass: "sorry"},
		{name: "consent redirect", url: "https://consent.google.com/ml", status: 200, wantClass: "consent"},
		{name: "rate limited", url: "https://www.google.com/maps/place/Coffee", status: 429, wantClass: "rate_limited"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			page := &botCheckTestPage{
				url:         tt.url,
				evaluateErr: errors.New("probe must not run"),
			}

			err := detectBotBlock(page, tt.status)

			if !errors.Is(err, ErrBotBlocked) {
				t.Fatalf("detectBotBlock() = %v, want ErrBotBlocked", err)
			}
			if page.contentCalls != 0 {
				t.Errorf("page.Content() called %d times, want 0", page.contentCalls)
			}
			if len(page.observed) != 1 || page.observed[0] != tt.wantClass {
				t.Errorf("observed classes = %#v, want [%s]", page.observed, tt.wantClass)
			}
		})
	}
}

// A probe that cannot answer must not be read as "no bot block detected" data:
// the page is still reported, just without signals.
func TestDetectBotBlockToleratesFailedProbe(t *testing.T) {
	page := &botCheckTestPage{
		url:         "https://www.google.com/maps/place/Coffee",
		evaluateErr: errors.New("Page crashed while evaluating"),
	}

	if err := detectBotBlock(page, 200); err != nil {
		t.Fatalf("detectBotBlock() = %v, want nil", err)
	}
	if len(page.observed) != 1 || page.observed[0] != "maps" {
		t.Errorf("observed classes = %#v, want [maps]", page.observed)
	}
	if page.observedSize != 0 {
		t.Errorf("content bytes = %d, want 0", page.observedSize)
	}
}

// ClickForce abandons its click goroutine at the hard ceiling while that
// goroutine still holds the page, so every later selector inherits a wedged CDP
// call. In the corpus this was near-total: 51 of 56 places that hard-timed-out
// then timed out on every remaining selector, at another full ceiling each.
func TestClickFirstMatchingStopsChainAfterHardTimeout(t *testing.T) {
	trace := &claimTrace{worker: 4, urlID: 31, attempt: 2, lang: "en", url: "https://example.test/place", started: time.Now()}
	ctx := withPlaceTrace(context.Background(), trace)
	page := &clickFirstMatchingTestPage{
		clickErrors: map[string]error{
			"first": &ClickHardTimeoutError{Selector: "first", Ceiling: 4800 * time.Millisecond},
		},
	}

	got := clickFirstMatching(ctx, page, "open", 5, []string{"first", "second", "third"}, time.Millisecond, time.Millisecond, 0, nil)

	if got != "" {
		t.Fatalf("clickFirstMatching() = %q, want no match", got)
	}
	if len(page.clicked) != 1 || page.clicked[0] != "first" {
		t.Fatalf("clicked = %#v, want only the wedged selector", page.clicked)
	}
	if detail := trace.snapshot().Detail; !strings.Contains(detail, "wedged=first") {
		t.Errorf("stage detail = %q, want it to record the wedged selector", detail)
	}
}

// A plain driver error is a selector miss, not a wedged page, so the chain must
// still fall through — and must say so, since these were previously the one
// outcome that left no trace in the logs at all.
func TestClickFirstMatchingContinuesAndLogsOnNonTimeoutClickError(t *testing.T) {
	logs := captureLog(t)
	trace := &claimTrace{worker: 1, urlID: 9, attempt: 1, lang: "zh-tw", url: "https://example.test/place", started: time.Now()}
	ctx := withPlaceTrace(context.Background(), trace)
	page := &clickFirstMatchingTestPage{
		clickErrors: map[string]error{"first": errors.New("element not interactable")},
	}

	got := clickFirstMatching(ctx, page, "open", 5, []string{"first", "second"}, time.Millisecond, time.Millisecond, 0, nil)

	if got != "second" {
		t.Fatalf("clickFirstMatching() = %q, want second", got)
	}
	logText := logs.String()
	for _, want := range []string{
		"DIAG event=click_error",
		`selector="first"`,
		`error="element not interactable"`,
		"stage=place.extra_reviews",
		`worker=1 url_id=9 attempt=1 lang="zh-tw"`,
	} {
		if !strings.Contains(logText, want) {
			t.Fatalf("click-error diagnostic missing %q:\n%s", want, logText)
		}
	}
}
