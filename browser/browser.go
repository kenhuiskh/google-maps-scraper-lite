package browser

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"os"
	"sync"

	"github.com/playwright-community/playwright-go"
)

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
}

// Options configures the browser.
type Options struct {
	Concurrency int
	Headless    bool
	Lang        string
}

// New launches playwright, creates a browser and a shared context. Pages are
// created lazily up to Concurrency, so the browser can ramp from one tab to the
// configured maximum as workers need them. All pages share the same context so
// cookies (needed for the reviews RPC credentials:include fetch) are shared
// across workers.
func New(opts Options) (*Browser, error) {
	// Skip install when PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD is set (e.g. CI with pre-installed browsers).
	if os.Getenv("PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD") == "" {
		if err := playwright.Install(); err != nil {
			return nil, fmt.Errorf("install playwright: %w", err)
		}
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
	pool <- page

	return &Browser{pw: pw, browser: br, context: ctx, pages: pool, created: 1, max: opts.Concurrency}, nil
}

// AcquirePage takes a page from the pool, creating a new tab lazily until the
// configured maximum is reached.
func (b *Browser) AcquirePage() playwright.Page {
	select {
	case page := <-b.pages:
		return page
	default:
	}

	b.mu.Lock()
	if b.created < b.max {
		b.created++
		b.mu.Unlock()
		page, err := b.context.NewPage()
		if err == nil {
			return page
		}
		b.mu.Lock()
		b.created--
		b.mu.Unlock()
	} else {
		b.mu.Unlock()
	}
	return <-b.pages
}

// ReleasePage returns a page to the pool. If the page has crashed or been
// closed, a fresh replacement is created so the pool size stays constant.
func (b *Browser) ReleasePage(page playwright.Page) {
	if page.IsClosed() {
		b.mu.Lock()
		if b.created > 0 {
			b.created--
		}
		b.mu.Unlock()
		return
	}
	b.pages <- page
}

// Close closes all pages, the browser context, the browser, and stops playwright.
func (b *Browser) Close() error {
	for len(b.pages) > 0 {
		page := <-b.pages
		_ = page.Close()
	}
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
