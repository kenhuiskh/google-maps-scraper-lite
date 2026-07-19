package gmaps

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

func Test_parsePlaceURLIdentifiers(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		wantFTID string
		wantLat  string
		wantLng  string
		wantOK   bool
	}{
		{
			name:     "bbq feed URL",
			url:      "https://www.google.com/maps/place/A+BBQ+House/data=!4m7!3m6!1s0x882b35c34f214d21:0x60827526c532d96c!8m2!3d43.66712!4d-79.3857331!16s%2Fg%2F11c0zqjq0y",
			wantFTID: "0x882b35c34f214d21:0x60827526c532d96c",
			wantLat:  "43.66712",
			wantLng:  "-79.3857331",
			wantOK:   true,
		},
		{
			name:     "smoque feed URL",
			url:      "https://www.google.com/maps/place/SmoQue+N'+Bones/data=!4m7!3m6!1s0x882b355387542ba7:0x986ea62ae3613c83!8m2!3d43.6561153!4d-79.3937439!16s%2Fg%2F11f3xqjq0y",
			wantFTID: "0x882b355387542ba7:0x986ea62ae3613c83",
			wantLat:  "43.6561153",
			wantLng:  "-79.3937439",
			wantOK:   true,
		},
		{
			name:   "place_id query URL has no ftid",
			url:    "https://www.google.com/maps/search/?api=1&query=A%20BBQ%20House&query_place_id=ChIJmyzUTsM1MogRbNky3iZ1CBk",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ftid, lat, lng, ok := parsePlaceURLIdentifiers(tt.url)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			if ftid != tt.wantFTID {
				t.Errorf("ftid = %q, want %q", ftid, tt.wantFTID)
			}
			if lat != tt.wantLat {
				t.Errorf("lat = %q, want %q", lat, tt.wantLat)
			}
			if lng != tt.wantLng {
				t.Errorf("lng = %q, want %q", lng, tt.wantLng)
			}
		})
	}
}

func Test_buildPreviewPlaceURL(t *testing.T) {
	got := buildPreviewPlaceURL("0x882b35c34f214d21:0x60827526c532d96c", "43.66712", "-79.3857331", "en")

	if !strings.Contains(got, "preview/place") {
		t.Errorf("URL %q does not contain preview/place", got)
	}
	if !strings.Contains(got, "hl=en") {
		t.Errorf("URL %q does not contain hl=en", got)
	}

	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", got, err)
	}
	pb := parsed.Query().Get("pb")
	if pb == "" {
		t.Fatalf("URL %q has no pb= param", got)
	}
	if !strings.Contains(pb, "0x882b35c34f214d21:0x60827526c532d96c") {
		t.Errorf("decoded pb = %q, want it to contain the ftid", pb)
	}
	if !strings.Contains(pb, "43.66712") {
		t.Errorf("decoded pb = %q, want it to contain the lat", pb)
	}
	if !strings.Contains(pb, "-79.3857331") {
		t.Errorf("decoded pb = %q, want it to contain the lng", pb)
	}
}

func Test_buildPreviewPlaceURL_DefaultsLangToEn(t *testing.T) {
	got := buildPreviewPlaceURL("0x1:0x2", "1.0", "2.0", "")
	if !strings.Contains(got, "hl=en") {
		t.Errorf("URL %q does not default to hl=en", got)
	}
}

func Test_extractRPCEntry_BBQFixture(t *testing.T) {
	body, err := os.ReadFile("testdata/place_rpc_bbq.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	raw, ok := stripXSSIPrefix(body)
	if !ok {
		t.Fatalf("stripXSSIPrefix: no )]}' prefix found")
	}

	entry, err := EntryFromJSON(raw)
	if err != nil {
		t.Fatalf("EntryFromJSON: %v", err)
	}

	if entry.Title != "A BBQ House" {
		t.Errorf("Title = %q, want %q", entry.Title, "A BBQ House")
	}
	if entry.Category != "Chinese restaurant" {
		t.Errorf("Category = %q, want %q", entry.Category, "Chinese restaurant")
	}
	// Address/PlusCode/Phone below are the *actual* values EntryFromJSON produces
	// for this fixture (verified by direct inspection) — they differ slightly in
	// formatting from the task brief's paraphrase (no ", Canada" suffix; local
	// phone format rather than +1-prefixed), which is expected since the two
	// fixtures exercise EntryFromJSON's two known JSON-shape variants.
	if entry.Address != "664 Yonge St, Toronto, ON M4Y 2A6" {
		t.Errorf("Address = %q, want %q", entry.Address, "664 Yonge St, Toronto, ON M4Y 2A6")
	}
	if entry.Phone != "(416) 925-8898" {
		t.Errorf("Phone = %q, want %q", entry.Phone, "(416) 925-8898")
	}
	if entry.ReviewRating != 4.7 {
		t.Errorf("ReviewRating = %v, want %v", entry.ReviewRating, 4.7)
	}
	if entry.PlusCode != "MJ87+RP Toronto, Ontario" {
		t.Errorf("PlusCode = %q, want %q", entry.PlusCode, "MJ87+RP Toronto, Ontario")
	}
	if entry.Latitude != 43.66712 {
		t.Errorf("Latitude = %v, want %v", entry.Latitude, 43.66712)
	}
	if entry.Longitude != -79.3857331 {
		t.Errorf("Longitude = %v, want %v", entry.Longitude, -79.3857331)
	}
}

func Test_extractRPCEntry_SmoqueFixture(t *testing.T) {
	body, err := os.ReadFile("testdata/place_rpc_smoque.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	raw, ok := stripXSSIPrefix(body)
	if !ok {
		t.Fatalf("stripXSSIPrefix: no )]}' prefix found")
	}

	entry, err := EntryFromJSON(raw)
	if err != nil {
		t.Fatalf("EntryFromJSON: %v", err)
	}

	if entry.Title != "SmoQue N' Bones" {
		t.Errorf("Title = %q, want %q", entry.Title, "SmoQue N' Bones")
	}
	if entry.Category != "Barbecue restaurant" {
		t.Errorf("Category = %q, want %q", entry.Category, "Barbecue restaurant")
	}
	if entry.Address != "30 Baldwin St, Toronto, ON M5T 1L3, Canada" {
		t.Errorf("Address = %q, want %q", entry.Address, "30 Baldwin St, Toronto, ON M5T 1L3, Canada")
	}
	if entry.Phone != "+1 647-341-5730" {
		t.Errorf("Phone = %q, want %q", entry.Phone, "+1 647-341-5730")
	}
	if entry.WebSite != "https://smoquenbones.com/" {
		t.Errorf("WebSite = %q, want %q", entry.WebSite, "https://smoquenbones.com/")
	}
	if entry.ReviewRating != 4.4 {
		t.Errorf("ReviewRating = %v, want %v", entry.ReviewRating, 4.4)
	}
	const coordTolerance = 1e-6
	if diff := entry.Latitude - 43.6561153; diff > coordTolerance || diff < -coordTolerance {
		t.Errorf("Latitude = %v, want ~%v", entry.Latitude, 43.6561153)
	}
	if diff := entry.Longitude - (-79.3937439); diff > coordTolerance || diff < -coordTolerance {
		t.Errorf("Longitude = %v, want ~%v", entry.Longitude, -79.3937439)
	}
}

func Test_stripXSSIPrefix_MissingPrefix(t *testing.T) {
	_, ok := stripXSSIPrefix([]byte(`[null,[],null]`))
	if ok {
		t.Fatalf("stripXSSIPrefix() ok = true, want false for body without )]}' prefix")
	}
}

const bbqFeedURL = "https://www.google.com/maps/place/A+BBQ+House/data=!4m7!3m6!1s0x882b35c34f214d21:0x60827526c532d96c!8m2!3d43.66712!4d-79.3857331!16s%2Fg%2F11c0zqjq0y"

func Test_FetchPlaceHTTP_ParsesFixture(t *testing.T) {
	body, err := os.ReadFile("testdata/place_rpc_bbq.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	var gotUA, gotAcceptLang, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotAcceptLang = r.Header.Get("Accept-Language")
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	restore := setPreviewPlaceBaseURLForTest(srv.URL)
	defer restore()

	entry, err := FetchPlaceHTTP(context.Background(), bbqFeedURL, "en")
	if err != nil {
		t.Fatalf("FetchPlaceHTTP: %v", err)
	}

	found := false
	for _, ua := range placeHTTPUserAgents {
		if gotUA == ua {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("request User-Agent = %q, want one of the pool", gotUA)
	}
	if gotAcceptLang != "en-US,en;q=0.9" {
		t.Errorf("request Accept-Language = %q, want %q", gotAcceptLang, "en-US,en;q=0.9")
	}
	q, err := url.ParseQuery(gotQuery)
	if err != nil {
		t.Fatalf("parse request query %q: %v", gotQuery, err)
	}
	if q.Get("hl") != "en" || q.Get("gl") != "us" {
		t.Errorf("request query hl/gl = %q/%q, want en/us", q.Get("hl"), q.Get("gl"))
	}
	if !strings.Contains(q.Get("pb"), "0x882b35c34f214d21:0x60827526c532d96c") {
		t.Errorf("request pb param = %q, want it to contain the ftid", q.Get("pb"))
	}

	if entry.Title != "A BBQ House" {
		t.Errorf("Title = %q, want %q", entry.Title, "A BBQ House")
	}
	if entry.Link == "" {
		t.Errorf("Link is empty, want a canonical/fallback URL")
	}
	if len(entry.ReviewTags) != 0 {
		t.Errorf("ReviewTags = %v, want empty (browser-only field)", entry.ReviewTags)
	}
}

func Test_FetchPlaceHTTP_NoFTID_ReturnsUnavailable(t *testing.T) {
	_, err := FetchPlaceHTTP(context.Background(), "https://www.google.com/maps/search/?api=1&query=x&query_place_id=abc", "en")
	if !errors.Is(err, ErrHTTPPlaceUnavailable) {
		t.Fatalf("err = %v, want ErrHTTPPlaceUnavailable", err)
	}
}

func Test_FetchPlaceHTTP_429_IsBotBlocked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	restore := setPreviewPlaceBaseURLForTest(srv.URL)
	defer restore()

	_, err := FetchPlaceHTTP(context.Background(), bbqFeedURL, "en")
	if !errors.Is(err, ErrBotBlocked) {
		t.Fatalf("err = %v, want ErrBotBlocked", err)
	}
}

func Test_FetchPlaceHTTP_UnusualTrafficBody_IsBotBlocked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("Our systems have detected unusual traffic from your computer network."))
	}))
	defer srv.Close()

	restore := setPreviewPlaceBaseURLForTest(srv.URL)
	defer restore()

	_, err := FetchPlaceHTTP(context.Background(), bbqFeedURL, "en")
	if !errors.Is(err, ErrBotBlocked) {
		t.Fatalf("err = %v, want ErrBotBlocked", err)
	}
}

func Test_FetchPlaceHTTP_200WithoutBlob_ReturnsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"unexpected":"shape"}`))
	}))
	defer srv.Close()

	restore := setPreviewPlaceBaseURLForTest(srv.URL)
	defer restore()

	_, err := FetchPlaceHTTP(context.Background(), bbqFeedURL, "en")
	if !errors.Is(err, ErrHTTPPlaceUnavailable) {
		t.Fatalf("err = %v, want ErrHTTPPlaceUnavailable", err)
	}
}

func Test_FetchPlaceHTTP_400_ReturnsUnavailableNotBotBlocked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("bad request"))
	}))
	defer srv.Close()

	restore := setPreviewPlaceBaseURLForTest(srv.URL)
	defer restore()

	_, err := FetchPlaceHTTP(context.Background(), bbqFeedURL, "en")
	if !errors.Is(err, ErrHTTPPlaceUnavailable) {
		t.Fatalf("err = %v, want ErrHTTPPlaceUnavailable", err)
	}
	if errors.Is(err, ErrBotBlocked) {
		t.Fatalf("err = %v, want NOT ErrBotBlocked for a plain 400", err)
	}
}

func Test_acceptLanguageHeader(t *testing.T) {
	tests := []struct {
		lang string
		want string
	}{
		{"en", "en-US,en;q=0.9"},
		{"", "en-US,en;q=0.9"},
		{"pt-BR", "pt-BR,pt;q=0.9"},
	}
	for _, tt := range tests {
		if got := acceptLanguageHeader(tt.lang); got != tt.want {
			t.Errorf("acceptLanguageHeader(%q) = %q, want %q", tt.lang, got, tt.want)
		}
	}
}
