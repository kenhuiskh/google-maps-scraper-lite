// Package browserrod is a go-rod-backed engine that drives the gmaps scraper
// through the gmaps.Page interface. It mirrors the pool semantics of the
// playwright-backed `browser` package exactly so either engine can be selected
// at the binary level without any change to gmaps orchestration.
package browserrod

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"math/big"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
	"github.com/go-rod/stealth"
	"github.com/gosom/google-maps-scraper-lite/gmaps"
)

// maxPageUses is the number of scrapes a single tab serves before it is closed
// and replaced; caps Chromium tab memory growth on long runs.
const maxPageUses = 40

// acquireTimeout bounds the blocking wait for a pooled page so workers surface
// an error instead of hanging forever when the browser has died.
const acquireTimeout = 90 * time.Second

const fallbackUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

var defaultUserAgents = []string{
	fallbackUserAgent,
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
	"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 14_4_1) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36",
}

// Browser manages the go-rod browser instance and a pool of pages. Its exported
// surface (New/AcquirePage/ReleasePage/RetirePage/Close) and pool behavior match
// the playwright-backed browser.Browser one-for-one.
type Browser struct {
	launcher *launcher.Launcher
	browser  *rod.Browser
	pages    chan gmaps.Page
	mu       sync.Mutex
	created  int
	max      int
	uses     map[gmaps.Page]int // scrapes served per page; guarded by mu

	locale          string
	disableBlocking bool
	blockedSet      map[string]struct{} // lowercased resource types to block
}

// Options configures the browser. Same fields as browser.Options so callers can
// construct either engine from one config.
type Options struct {
	Concurrency             int
	Headless                bool
	Lang                    string
	DisableResourceBlocking bool
	BlockedResourceTypes    []string
}

// New launches a go-rod-controlled Chromium and seeds the pool with one page.
// Pages are created lazily up to Concurrency, mirroring browser.New. All pages
// belong to the same browser so cookies (needed for the reviews RPC
// credentials:include fetch) are shared across workers, matching the shared
// context in the playwright engine.
func New(opts Options) (*Browser, error) {
	l := launcher.New().
		Headless(opts.Headless).
		Set("disable-blink-features", "AutomationControlled").
		Set("disable-dev-shm-usage").
		Set("disable-gpu").
		Set("disable-background-networking").
		Set("no-first-run").
		Set("js-flags", "--max-old-space-size=256")

	// Reuse an already-installed Chromium instead of downloading one. An explicit
	// ROD_BROWSER_BIN wins (parity with browser.go honoring a playwright env var),
	// otherwise fall back to whatever LookPath finds on the host.
	if bin := os.Getenv("ROD_BROWSER_BIN"); bin != "" {
		l = l.Bin(bin)
	} else if bin, ok := launcher.LookPath(); ok {
		l = l.Bin(bin)
	}

	controlURL, err := l.Launch()
	if err != nil {
		return nil, fmt.Errorf("launch rod: %w", err)
	}

	br := rod.New().ControlURL(controlURL)
	if err := br.Connect(); err != nil {
		l.Cleanup()
		return nil, fmt.Errorf("connect rod: %w", err)
	}

	locale := opts.Lang
	if locale == "" {
		locale = "en-US"
	}
	if opts.Concurrency < 1 {
		opts.Concurrency = 1
	}

	b := &Browser{
		launcher:        l,
		browser:         br,
		pages:           make(chan gmaps.Page, opts.Concurrency),
		max:             opts.Concurrency,
		uses:            make(map[gmaps.Page]int),
		locale:          locale,
		disableBlocking: opts.DisableResourceBlocking,
		blockedSet:      buildBlockedSet(opts.BlockedResourceTypes),
	}

	page, err := b.newPage()
	if err != nil {
		_ = br.Close()
		l.Cleanup()
		return nil, err
	}
	b.pages <- gmaps.Page(page)
	b.created = 1

	return b, nil
}

// buildBlockedSet lowercases the requested resource types so the same
// ["image","media","font"] config blocks the same resources despite rod
// reporting capitalized type names (Image/Media/Font).
func buildBlockedSet(blockedTypes []string) map[string]struct{} {
	if len(blockedTypes) == 0 {
		blockedTypes = []string{"image", "media", "font"}
	}
	set := make(map[string]struct{}, len(blockedTypes))
	for _, typ := range blockedTypes {
		set[strings.ToLower(typ)] = struct{}{}
	}
	return set
}

// newPage creates a fresh tab, applies stealth + UA/viewport, and installs
// resource blocking. Used by New, lazy-grow, and replenish.
func (b *Browser) newPage() (*rodPage, error) {
	// stealth.Page injects the anti-detection init script (a superset of the
	// single navigator.webdriver override the playwright engine uses) on every
	// new document, before any navigation. Real Chromium + this script is what
	// clears the Google Maps "enable JavaScript" wall that killed the Obscura and
	// Lightpanda experiments.
	page, err := stealth.Page(b.browser)
	if err != nil {
		return nil, fmt.Errorf("new page: %w", err)
	}

	if err := page.SetUserAgent(&proto.NetworkSetUserAgentOverride{
		UserAgent:      randomUserAgent(),
		AcceptLanguage: b.locale,
	}); err != nil {
		_ = page.Close()
		return nil, fmt.Errorf("set user agent: %w", err)
	}

	if err := page.SetViewport(&proto.EmulationSetDeviceMetricsOverride{
		Width:             1280,
		Height:            800,
		DeviceScaleFactor: 1,
	}); err != nil {
		_ = page.Close()
		return nil, fmt.Errorf("set viewport: %w", err)
	}

	rp := &rodPage{page: page}

	if !b.disableBlocking {
		router := page.HijackRequests()
		blocked := b.blockedSet
		if err := router.Add("*", "", func(h *rod.Hijack) {
			if _, drop := blocked[strings.ToLower(string(h.Request.Type()))]; drop {
				h.Response.Fail(proto.NetworkErrorReasonBlockedByClient)
				return
			}
			h.ContinueRequest(&proto.FetchContinueRequest{})
		}); err != nil {
			_ = page.Close()
			return nil, fmt.Errorf("install resource blocking: %w", err)
		}
		rp.router = router
		go router.Run()
	}

	return rp, nil
}

// AcquirePage takes a page from the pool, creating a new tab lazily until the
// configured maximum is reached. It never blocks past acquireTimeout: a dead
// browser must surface an error rather than wedge every worker silently.
func (b *Browser) AcquirePage(ctx context.Context) (gmaps.Page, error) {
	// Fast path: take a pooled page without blocking, discarding dead ones.
	for {
		var page gmaps.Page
		select {
		case page = <-b.pages:
		default:
		}
		if page == nil {
			break
		}
		if page.IsClosed() {
			b.discardPage(page)
			continue
		}
		return page, nil
	}

	b.mu.Lock()
	if b.created < b.max {
		b.created++
		b.mu.Unlock()
		page, err := b.newPage()
		if err != nil {
			b.mu.Lock()
			b.created--
			b.mu.Unlock()
			return nil, fmt.Errorf("acquire page: %w", err)
		}
		return page, nil
	}
	b.mu.Unlock()

	timeout := time.NewTimer(acquireTimeout)
	defer timeout.Stop()
	for {
		select {
		case page := <-b.pages:
			if page.IsClosed() {
				b.discardPage(page)
				continue
			}
			return page, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timeout.C:
			return nil, fmt.Errorf("acquire page: timed out after %s — browser may be dead", acquireTimeout)
		}
	}
}

// discardPage forgets a dead page: its use count is dropped and its created
// slot is released so a replacement can be created lazily.
func (b *Browser) discardPage(page gmaps.Page) {
	b.mu.Lock()
	delete(b.uses, page)
	if b.created > 0 {
		b.created--
	}
	b.mu.Unlock()
}

// ReleasePage returns a page to the pool. Dead pages and pages that served
// maxPageUses scrapes are replaced with fresh ones so the pool never silently
// drains and tab memory stays bounded.
func (b *Browser) ReleasePage(page gmaps.Page) {
	if page.IsClosed() {
		b.mu.Lock()
		delete(b.uses, page)
		b.mu.Unlock()
		b.replenish()
		return
	}

	b.mu.Lock()
	b.uses[page]++
	worn := b.uses[page] >= maxPageUses
	if worn {
		delete(b.uses, page)
	}
	b.mu.Unlock()

	if worn {
		_ = page.Close()
		b.replenish()
		return
	}
	b.pages <- page
}

// RetirePage closes a tainted page instead of returning it to the pool. Use it
// after bot blocks, watchdog timeouts, or page crashes so the next claim gets a
// clean tab.
func (b *Browser) RetirePage(page gmaps.Page) {
	if page == nil {
		return
	}
	_ = page.Close()
	b.mu.Lock()
	delete(b.uses, page)
	b.mu.Unlock()
	b.replenish()
}

// replenish creates a fresh page taking over the created slot of a dead or
// retired one. On failure the slot is released so the pool shrinks by one
// instead of counting a page that no longer exists.
func (b *Browser) replenish() {
	page, err := b.newPage()
	if err != nil {
		b.mu.Lock()
		if b.created > 0 {
			b.created--
		}
		b.mu.Unlock()
		log.Printf("browser pool: replacement page failed: %v", err)
		return
	}
	gp := gmaps.Page(page)
	select {
	case b.pages <- gp:
	default:
		// Pool already at capacity; the slot was double-counted.
		_ = gp.Close()
		b.mu.Lock()
		if b.created > 0 {
			b.created--
		}
		b.mu.Unlock()
	}
}

// Close closes all pooled pages, the browser, and cleans up the launcher.
func (b *Browser) Close() error {
	for len(b.pages) > 0 {
		page := <-b.pages
		_ = page.Close()
	}
	b.mu.Lock()
	b.uses = make(map[gmaps.Page]int)
	b.mu.Unlock()
	err := b.browser.Close()
	b.launcher.Cleanup()
	return err
}

func randomUserAgent() string {
	return randomUserAgentFrom(defaultUserAgents)
}

func randomUserAgentFrom(pool []string) string {
	if len(pool) == 0 {
		return fallbackUserAgent
	}

	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(pool))))
	if err != nil {
		return pool[0]
	}
	return pool[n.Int64()]
}
