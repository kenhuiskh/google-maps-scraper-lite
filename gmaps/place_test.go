package gmaps

import (
	"errors"
	"net/url"
	"reflect"
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
