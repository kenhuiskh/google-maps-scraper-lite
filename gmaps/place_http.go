package gmaps

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// ErrHTTPPlaceUnavailable is returned by FetchPlaceHTTP when the browser-free
// preview/place RPC path cannot be used for the given URL (no ftid present)
// or does not yield a parseable place blob. Callers should fall back to the
// Playwright browser path (ScrapePlace) in this case — it does not indicate
// a bot block.
var ErrHTTPPlaceUnavailable = errors.New("http place path unavailable, use browser")

// previewPlacePBTemplate is the fixed field-flags payload for Google Maps'
// undocumented `preview/place` RPC, with {FTID}/{LAT}/{LNG} placeholders.
// Captured and verified live against real place pages on 2026-07-16; the
// session-token/business-name/fid/place_id groups present in a full browser
// request have been stripped out — they are not required for a lookup keyed
// solely by ftid. Source: .superpowers/sdd/rpc-fixtures/pb_template_min.txt.
// This is opaque Google internal wire format — do not attempt to "clean up"
// or further minimize it.
const previewPlacePBTemplate = `!1m22!1s{FTID}!3m12!1m3!1d23088.89672562648!2d{LNG}!3d{LAT}!2m3!1f0.0!2f0.0!3f0.0!3m2!1i1024!2i768!4f13.1!4m2!3d{LAT}!4d{LNG}!12m4!2m3!1i360!2i120!4i8!13m57!2m2!1i203!2i100!3m2!2i4!5b1!6m6!1m2!1i86!2i86!1m2!1i408!2i240!7m33!1m3!1e1!2b0!3e3!1m3!1e2!2b1!3e2!1m3!1e2!2b0!3e3!1m3!1e8!2b0!3e3!1m3!1e10!2b0!3e3!1m3!1e10!2b1!3e2!1m3!1e10!2b0!3e4!1m3!1e9!2b1!3e2!2b1!9b0!15m8!1m7!1m2!1m1!1e2!2m2!1i195!2i195!3i20!15m108!1m26!13m9!2b1!3b1!4b1!6i1!8b1!9b1!14b1!20b1!25b1!18m15!3b1!4b1!5b1!6b1!13b1!14b1!17b1!21b1!22b1!30b1!32b1!33m1!1b1!34b1!36e2!10m1!8e3!11m1!3e1!17b1!20m2!1e3!1e6!24b1!25b1!26b1!27b1!29b1!30m1!2b1!36b1!37b1!39m3!2m2!2i1!3i1!43b1!52b1!54m1!1b1!55b1!56m1!1b1!61m2!1m1!1e1!65m5!3m4!1m3!1m2!1i224!2i298!72m22!1m8!2b1!5b1!7b1!12m4!1b1!2b1!4m1!1e1!4b1!8m10!1m6!4m1!1e1!4m1!1e3!4m1!1e4!3sother_user_google_review_posts__and__hotel_and_vr_partner_review_posts!6m1!1e1!9b1!89b1!90m2!1m1!1e2!98m3!1b1!2b1!3b1!103b1!113b1!114m3!1b1!2m1!1b1!117b1!122m1!1b1!126b1!127b1!128m1!1b0!21m0!22m1!1e81!30m8!3b1!6m2!1b1!2b1!7m2!1e3!2b1!9b1!34m5!7b1!10b1!14b1!15m1!1b0!37i786`

// previewPlaceBaseURL is the preview/place RPC endpoint. It is a var (not a
// const) so tests can redirect it at an httptest.Server instead of the network.
var previewPlaceBaseURL = "https://www.google.com/maps/preview/place"

// setPreviewPlaceBaseURLForTest points the RPC base URL at addr and returns a
// func that restores the original value. Test-only helper.
func setPreviewPlaceBaseURLForTest(addr string) (restore func()) {
	prev := previewPlaceBaseURL
	previewPlaceBaseURL = addr
	return func() { previewPlaceBaseURL = prev }
}

// placeHTTPUserAgents mirrors browser.defaultUserAgents. Kept as an
// independent copy per task brief — gmaps must not import the browser
// package (that would create an import cycle with browser -> gmaps callers).
var placeHTTPUserAgents = []string{
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
	"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 14_4_1) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36",
}

// placeHTTPClient is shared across all FetchPlaceHTTP calls. No cookie jar:
// the RPC works fine without one (verified live 2026-07-16).
var placeHTTPClient = &http.Client{
	Timeout: 30 * time.Second,
	Transport: &http.Transport{
		MaxIdleConnsPerHost: 32,
		ForceAttemptHTTP2:   true,
	},
}

// maxPlaceHTTPBodyBytes caps how much of the RPC response body is read.
// Real responses are ~20-24KB; this leaves generous headroom.
const maxPlaceHTTPBodyBytes = 2 << 20 // 2 MiB

const xssiPrefix = `)]}'`

var (
	ftidURLRE = regexp.MustCompile(`!1s(0x[0-9a-fA-F]+:0x[0-9a-fA-F]+)`)
	latURLRE  = regexp.MustCompile(`!3d(-?[0-9.]+)`)
	lngURLRE  = regexp.MustCompile(`!4d(-?[0-9.]+)`)
)

// parsePlaceURLIdentifiers extracts the feature id and coordinates from a
// Google Maps place URL's data= segment. ok is false if the ftid is absent
// (e.g. a place_id: query URL) — caller must fall back to the browser path.
func parsePlaceURLIdentifiers(placeURL string) (ftid, lat, lng string, ok bool) {
	m := ftidURLRE.FindStringSubmatch(placeURL)
	if m == nil {
		return "", "", "", false
	}
	ftid = m[1]

	if lm := latURLRE.FindStringSubmatch(placeURL); lm != nil {
		lat = lm[1]
	}
	if lm := lngURLRE.FindStringSubmatch(placeURL); lm != nil {
		lng = lm[1]
	}

	return ftid, lat, lng, true
}

// buildPreviewPlaceURL fills the pb template with ftid/lat/lng, URL-encodes
// it, and assembles the full preview/place RPC URL. lang defaults to "en".
func buildPreviewPlaceURL(ftid, lat, lng, lang string) string {
	if lang == "" {
		lang = "en"
	}

	pb := previewPlacePBTemplate
	pb = strings.ReplaceAll(pb, "{FTID}", ftid)
	pb = strings.ReplaceAll(pb, "{LAT}", lat)
	pb = strings.ReplaceAll(pb, "{LNG}", lng)

	return fmt.Sprintf("%s?hl=%s&gl=us&pb=%s", previewPlaceBaseURL, url.QueryEscape(lang), url.QueryEscape(pb))
}

// acceptLanguageHeader builds an Accept-Language header value from a Google
// Maps lang code. "en" -> "en-US,en;q=0.9"; a regioned code like "pt-BR" ->
// "pt-BR,pt;q=0.9".
func acceptLanguageHeader(lang string) string {
	if lang == "" {
		lang = "en"
	}
	if base, _, found := strings.Cut(lang, "-"); found {
		return fmt.Sprintf("%s,%s;q=0.9", lang, base)
	}
	return fmt.Sprintf("%s-US,%s;q=0.9", lang, lang)
}

func randomPlaceHTTPUserAgent() string {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(placeHTTPUserAgents))))
	if err != nil {
		return placeHTTPUserAgents[0]
	}
	return placeHTTPUserAgents[n.Int64()]
}

// stripXSSIPrefix removes the `)]}'` XSSI-protection prefix (and the
// whitespace/newline that follows it) that Google prepends to the RPC JSON
// body. ok is false if the prefix is absent.
func stripXSSIPrefix(body []byte) (raw []byte, ok bool) {
	if !bytes.HasPrefix(body, []byte(xssiPrefix)) {
		return nil, false
	}
	trimmed := bytes.TrimSpace(bytes.TrimPrefix(body, []byte(xssiPrefix)))
	return trimmed, true
}

// detectHTTPBotBlock inspects an RPC response for signs Google is blocking
// this client: HTTP 429, a redirect to a captcha/consent/sorry wall, or
// "unusual traffic" / "automated queries" text in the body.
func detectHTTPBotBlock(resp *http.Response, body []byte) error {
	if resp.StatusCode == http.StatusTooManyRequests {
		return fmt.Errorf("bot block: HTTP 429: %w", ErrBotBlocked)
	}

	if resp.Request != nil && resp.Request.URL != nil {
		finalURL := resp.Request.URL.String()
		for _, marker := range []string{"/sorry/", "consent.google", "ipv4.google.com/sorry"} {
			if strings.Contains(finalURL, marker) {
				return fmt.Errorf("bot block: url %q: %w", finalURL, ErrBotBlocked)
			}
		}
	}

	lc := strings.ToLower(string(body))
	if strings.Contains(lc, "unusual traffic") || strings.Contains(lc, "automated queries") {
		return fmt.Errorf("bot block: unusual-traffic body: %w", ErrBotBlocked)
	}

	return nil
}

// FetchPlaceHTTP fetches place details over the preview/place RPC without a
// browser. Returns ErrHTTPPlaceUnavailable (fall back to browser) if the URL
// lacks an ftid or the RPC yields no parseable blob, or a wrapped
// ErrBotBlocked if Google is blocking this client.
func FetchPlaceHTTP(ctx context.Context, placeURL string, lang string) (*Entry, error) {
	ftid, lat, lng, ok := parsePlaceURLIdentifiers(placeURL)
	if !ok {
		return nil, fmt.Errorf("place URL has no ftid: %w", ErrHTTPPlaceUnavailable)
	}

	rpcURL := buildPreviewPlaceURL(ftid, lat, lng, lang)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rpcURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create preview/place request: %w", err)
	}
	req.Header.Set("User-Agent", randomPlaceHTTPUserAgent())
	req.Header.Set("Accept-Language", acceptLanguageHeader(lang))
	req.Header.Set("Accept", "*/*")

	resp, err := placeHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("preview/place request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPlaceHTTPBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("read preview/place response: %w", err)
	}

	if berr := detectHTTPBotBlock(resp, body); berr != nil {
		return nil, berr
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("preview/place status %d: %w", resp.StatusCode, ErrHTTPPlaceUnavailable)
	}

	raw, ok := stripXSSIPrefix(body)
	if !ok {
		return nil, fmt.Errorf("preview/place response missing )]}' blob: %w", ErrHTTPPlaceUnavailable)
	}

	entry, err := EntryFromJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("parse preview/place entry: %v: %w", err, ErrHTTPPlaceUnavailable)
	}

	entry.Link = choosePlaceLink(entry.Link, "", placeURLWithLang(placeURL, lang))

	return &entry, nil
}

// ScrapePlaceHTTP fetches place details over HTTP (no browser) and, when opts
// requests it, extracts emails — mirroring ScrapePlace's post-parse email step.
// It cannot fetch extra reviews; callers wanting opts.ExtraReviews>0 must use the
// browser path.
func ScrapePlaceHTTP(ctx context.Context, placeURL string, opts PlaceOptions) (*Entry, error) {
	entry, err := FetchPlaceHTTP(ctx, placeURL, opts.LangCode)
	if err != nil {
		return nil, err
	}
	if opts.ExtractEmail && entry.IsWebsiteValidForEmail() {
		websiteURL := normalizeGoogleURL(entry.WebSite)
		if emails, eerr := ExtractEmails(ctx, websiteURL); eerr == nil {
			entry.Emails = emails
		}
	}
	return entry, nil
}
