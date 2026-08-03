package gmaps

import (
	"context"
	"errors"
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
	if page.waitSelector != `[aria-label="Refine reviews"]` {
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
	want := []string{
		`button[aria-label*="review" i][jsaction]`,
		`button[aria-label*="reviews" i]`,
		`[role="main"] button[data-tab-index="1"]`,
		`[role="main"] a[href*="reviews"]`,
		`button[aria-label*="Sort reviews" i]`,
		`button[aria-label*="Sort" i]`,
		`[role="main"] button[data-value="Sort reviews"]`,
		`button[aria-label*="Newest" i]`,
		`[role="menuitemradio"][aria-label*="Newest" i]`,
		`[role="menu"] [role="menuitemradio"][data-index="1"]`,
		`[role="menu"] [role="menuitemradio"][data-value="2"]`,
	}
	clickErrors := make(map[string]error, len(want))
	for _, selector := range want {
		clickErrors[selector] = errors.New("selector miss")
	}
	page := &clickFirstMatchingTestPage{clickErrors: clickErrors}

	_ = scrapeExtraReviews(context.Background(), page, &Entry{}, 1)

	if !reflect.DeepEqual(page.clicked, want) {
		t.Fatalf("fallback selectors passed to ClickForce = %#v, want %#v", page.clicked, want)
	}
	for _, selector := range page.clicked {
		if selector == `[data-tab-index="1"]` || selector == `[data-index="1"]` || selector == `[data-value="2"]` {
			t.Fatalf("bare fallback selector passed to ClickForce: %q", selector)
		}
	}
}

type clickFirstMatchingTestPage struct {
	clickErrors   map[string]error
	evaluateErr   error
	evaluateCalls int
	clicked       []string
	sleeps        []time.Duration
	detailAtClick []string
	onClick       func(string)
}

func (p *clickFirstMatchingTestPage) Goto(string) (int, error)                 { return 200, nil }
func (p *clickFirstMatchingTestPage) Reload() (int, error)                     { return 200, nil }
func (p *clickFirstMatchingTestPage) Content() (string, error)                 { return "", nil }
func (p *clickFirstMatchingTestPage) URL() string                              { return "" }
func (p *clickFirstMatchingTestPage) Close() error                             { return nil }
func (p *clickFirstMatchingTestPage) IsClosed() bool                           { return false }
func (p *clickFirstMatchingTestPage) WaitSelector(string, time.Duration) error { return nil }
func (p *clickFirstMatchingTestPage) Sleep(d time.Duration)                    { p.sleeps = append(p.sleeps, d) }
func (p *clickFirstMatchingTestPage) Evaluate(string) (any, error) {
	p.evaluateCalls++
	if p.evaluateErr != nil {
		return nil, p.evaluateErr
	}
	return map[string]any{"count": float64(1)}, nil
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
	got := clickFirstMatching(ctx, page, "open", 5, []string{"first", "second", "third"}, time.Millisecond, time.Millisecond, 0)
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
	got := clickFirstMatching(context.Background(), page, "open", 5, []string{"first", "second"}, time.Millisecond, time.Millisecond, 0)
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

	if got := clickFirstMatching(ctx, page, "open", 5, []string{"review-tab"}, time.Millisecond, time.Millisecond, 0); got != "" {
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
