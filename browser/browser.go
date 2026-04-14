package browser

import (
	"fmt"
	"os"

	"github.com/playwright-community/playwright-go"
)

// Browser manages the playwright browser instance and a pool of pages.
type Browser struct {
	pw      *playwright.Playwright
	browser playwright.Browser
	context playwright.BrowserContext
	pages   chan playwright.Page
}

// Options configures the browser.
type Options struct {
	Concurrency int
	Headless    bool
	Lang        string
}

// New launches playwright, creates a browser and a shared context, and pre-creates
// Concurrency pages as a pool. All pages share the same context so cookies (needed
// for the reviews RPC credentials:include fetch) are shared across workers.
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
		UserAgent: playwright.String("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"),
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
	for i := 0; i < opts.Concurrency; i++ {
		page, err := ctx.NewPage()
		if err != nil {
			_ = ctx.Close()
			_ = br.Close()
			_ = pw.Stop()
			return nil, fmt.Errorf("new page %d: %w", i, err)
		}
		pool <- page
	}

	return &Browser{pw: pw, browser: br, context: ctx, pages: pool}, nil
}

// AcquirePage takes a page from the pool (blocks until one is available).
func (b *Browser) AcquirePage() playwright.Page {
	return <-b.pages
}

// ReleasePage returns a page to the pool. If the page has crashed or been
// closed, a fresh replacement is created so the pool size stays constant.
func (b *Browser) ReleasePage(page playwright.Page) {
	if page.IsClosed() {
		newPage, err := b.context.NewPage()
		if err != nil {
			// Can't recreate — shrink the pool rather than block forever.
			return
		}
		b.pages <- newPage
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
