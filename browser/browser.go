package browser

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"math/big"
	"os"
	"sync"
	"time"

	"github.com/mxschmitt/playwright-go"
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

// Browser manages the playwright browser instance and a pool of pages.
type Browser struct {
	pw      *playwright.Playwright
	browser playwright.Browser
	context playwright.BrowserContext
	pages   chan playwright.Page
	mu      sync.Mutex
	created int
	max     int
	uses    map[playwright.Page]int // scrapes served per page; guarded by mu
}

// Options configures the browser.
type Options struct {
	Concurrency             int
	Headless                bool
	Lang                    string
	DisableResourceBlocking bool
	BlockedResourceTypes    []string
}

// New launches playwright, creates a browser and a shared context. Pages are
// created lazily up to Concurrency, so the browser can ramp from one tab to the
// configured maximum as workers need them. All pages share the same context so
// cookies (needed for the reviews RPC credentials:include fetch) are shared
// across workers.
func New(opts Options) (*Browser, error) {
	installOpts := &playwright.RunOptions{Browsers: []string{"chromium"}}
	if os.Getenv("PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD") != "" {
		installOpts.SkipInstallBrowsers = true
	}
	if err := playwright.Install(installOpts); err != nil {
		return nil, fmt.Errorf("install playwright: %w", err)
	}

	pw, err := playwright.Run()
	if err != nil {
		return nil, fmt.Errorf("run playwright: %w", err)
	}

	br, err := pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{
		Headless: playwright.Bool(opts.Headless),
		Args: []string{
			"--disable-blink-features=AutomationControlled",
			"--disable-dev-shm-usage",
			"--disable-gpu",
			"--disable-background-networking",
			"--no-first-run",
			"--js-flags=--max-old-space-size=256",
		},
	})
	if err != nil {
		_ = pw.Stop()
		return nil, fmt.Errorf("launch browser: %w", err)
	}

	locale := opts.Lang
	if locale == "" {
		locale = "en-US"
	}
	ctx, err := br.NewContext(playwright.BrowserNewContextOptions{
		Locale:    playwright.String(locale),
		UserAgent: playwright.String(randomUserAgent()),
		Viewport:  &playwright.Size{Width: 1280, Height: 800},
	})
	if err != nil {
		_ = br.Close()
		_ = pw.Stop()
		return nil, fmt.Errorf("new context: %w", err)
	}

	if err := ctx.AddInitScript(playwright.Script{
		Content: playwright.String(`Object.defineProperty(navigator, 'webdriver', {get: () => undefined})`),
	}); err != nil {
		_ = ctx.Close()
		_ = br.Close()
		_ = pw.Stop()
		return nil, fmt.Errorf("add init script: %w", err)
	}
	if !opts.DisableResourceBlocking {
		if err := installResourceBlocking(ctx, opts.BlockedResourceTypes); err != nil {
			_ = ctx.Close()
			_ = br.Close()
			_ = pw.Stop()
			return nil, fmt.Errorf("install resource blocking: %w", err)
		}
	}

	if opts.Concurrency < 1 {
		opts.Concurrency = 1
	}

	pool := make(chan playwright.Page, opts.Concurrency)
	page, err := ctx.NewPage()
	if err != nil {
		_ = ctx.Close()
		_ = br.Close()
		_ = pw.Stop()
		return nil, fmt.Errorf("new page: %w", err)
	}
	configurePage(page)
	pool <- page

	return &Browser{pw: pw, browser: br, context: ctx, pages: pool, created: 1, max: opts.Concurrency, uses: make(map[playwright.Page]int)}, nil
}

// configurePage sets default timeouts so playwright operations error out
// instead of blocking a worker indefinitely when Chromium is wedged.
func configurePage(page playwright.Page) {
	page.SetDefaultTimeout(30_000)
	page.SetDefaultNavigationTimeout(60_000)
}

func installResourceBlocking(ctx playwright.BrowserContext, blockedTypes []string) error {
	if len(blockedTypes) == 0 {
		blockedTypes = []string{"image", "media", "font"}
	}
	blocked := make(map[string]struct{}, len(blockedTypes))
	for _, typ := range blockedTypes {
		blocked[typ] = struct{}{}
	}
	return ctx.Route("**/*", func(route playwright.Route) {
		if _, ok := blocked[route.Request().ResourceType()]; ok {
			_ = route.Abort()
			return
		}
		_ = route.Continue()
	})
}

// AcquirePage takes a page from the pool, creating a new tab lazily until the
// configured maximum is reached. It never blocks past acquireTimeout: a dead
// browser must surface an error rather than wedge every worker silently.
func (b *Browser) AcquirePage(ctx context.Context) (playwright.Page, error) {
	// Fast path: take a pooled page without blocking, discarding dead ones.
	for {
		var page playwright.Page
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
		page, err := b.context.NewPage()
		if err != nil {
			b.mu.Lock()
			b.created--
			b.mu.Unlock()
			return nil, fmt.Errorf("acquire page: %w", err)
		}
		configurePage(page)
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
func (b *Browser) discardPage(page playwright.Page) {
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
func (b *Browser) ReleasePage(page playwright.Page) {
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
// clean tab in the shared context.
func (b *Browser) RetirePage(page playwright.Page) {
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
	page, err := b.context.NewPage()
	if err != nil {
		b.mu.Lock()
		if b.created > 0 {
			b.created--
		}
		b.mu.Unlock()
		log.Printf("browser pool: replacement page failed: %v", err)
		return
	}
	configurePage(page)
	select {
	case b.pages <- page:
	default:
		// Pool already at capacity; the slot was double-counted.
		_ = page.Close()
		b.mu.Lock()
		if b.created > 0 {
			b.created--
		}
		b.mu.Unlock()
	}
}

// Close closes all pages, the browser context, the browser, and stops playwright.
func (b *Browser) Close() error {
	for len(b.pages) > 0 {
		page := <-b.pages
		_ = page.Close()
	}
	b.mu.Lock()
	b.uses = make(map[playwright.Page]int)
	b.mu.Unlock()
	_ = b.context.Close()
	_ = b.browser.Close()
	return b.pw.Stop()
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
