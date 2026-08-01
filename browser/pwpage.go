package browser

import (
	"sync"
	"time"

	"github.com/gosom/google-maps-scraper-lite/gmaps"
	"github.com/mxschmitt/playwright-go"
)

// pwPage adapts a playwright.Page to the engine-neutral gmaps.Page interface.
// All playwright option structs live here so the gmaps package stays
// engine-neutral.
type pwPage struct {
	page     playwright.Page
	diagOnce sync.Once
	diag     *gmaps.PageDiagnosticsState
}

func newPWPage(page playwright.Page) *pwPage {
	return &pwPage{page: page, diag: gmaps.NewPageDiagnosticsState("playwright")}
}

func (p *pwPage) diagnosticState() *gmaps.PageDiagnosticsState {
	p.diagOnce.Do(func() {
		if p.diag == nil {
			p.diag = gmaps.NewPageDiagnosticsState("playwright")
		}
	})
	return p.diag
}

func (p *pwPage) Goto(u string) (status int, err error) {
	done := p.diagnosticState().BeginOperation("goto", u)
	defer func() { done(status, err) }()
	resp, err := p.page.Goto(u, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateCommit,
	})
	if err != nil {
		return 0, err
	}
	if resp == nil {
		return 0, nil
	}
	return resp.Status(), nil
}

func (p *pwPage) Reload() (status int, err error) {
	done := p.diagnosticState().BeginOperation("reload", "")
	defer func() { done(status, err) }()
	resp, err := p.page.Reload(playwright.PageReloadOptions{
		WaitUntil: playwright.WaitUntilStateCommit,
	})
	if err != nil {
		return 0, err
	}
	if resp == nil {
		return 0, nil
	}
	return resp.Status(), nil
}

func (p *pwPage) Content() (content string, err error) {
	done := p.diagnosticState().BeginOperation("content", "")
	defer func() { done(0, err) }()
	return p.page.Content()
}

func (p *pwPage) Evaluate(js string) (result any, err error) {
	done := p.diagnosticState().BeginOperation("evaluate", "")
	defer func() { done(0, err) }()
	return p.page.Evaluate(js)
}

// WaitSelector maps to Locator.WaitFor(Attached) rather than the "visible"
// default that playwright's own WaitForSelector uses. This is a deliberate,
// documented semantic nuance: the feed div is visible whenever it's attached,
// so this keeps a single wait primitive for both engines without changing
// observed behavior.
func (p *pwPage) WaitSelector(selector string, timeout time.Duration) (err error) {
	done := p.diagnosticState().BeginOperation("wait_selector", "")
	defer func() { done(0, err) }()
	return p.page.Locator(selector).First().WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateAttached,
		Timeout: playwright.Float(float64(timeout.Milliseconds())),
	})
}

func (p *pwPage) ClickForce(selector string, waitTimeout, clickTimeout time.Duration) (err error) {
	done := p.diagnosticState().BeginOperation("click", "")
	defer func() { done(0, err) }()
	loc := p.page.Locator(selector).First()
	if err := loc.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateAttached,
		Timeout: playwright.Float(float64(waitTimeout.Milliseconds())),
	}); err != nil {
		return err
	}
	return loc.Click(playwright.LocatorClickOptions{
		Timeout: playwright.Float(float64(clickTimeout.Milliseconds())),
		Force:   playwright.Bool(true),
	})
}

func (p *pwPage) URL() string {
	done := p.diagnosticState().BeginOperation("url", "")
	value := p.page.URL()
	done(0, nil)
	return value
}

func (p *pwPage) Sleep(d time.Duration) {
	p.page.WaitForTimeout(float64(d.Milliseconds()))
}

func (p *pwPage) Close() (err error) {
	done := p.diagnosticState().BeginOperation("close", "")
	defer func() {
		done(0, err)
		p.diagnosticState().MarkClosed()
	}()
	return p.page.Close()
}

func (p *pwPage) IsClosed() bool {
	return p.page.IsClosed()
}

func (p *pwPage) DiagnosticSnapshot() gmaps.PageDiagnosticSnapshot {
	return p.diagnosticState().Snapshot(p.page.IsClosed())
}

func (p *pwPage) ObservePageDiagnostics(class string, contentBytes int, title string) {
	p.diagnosticState().ObservePage(class, contentBytes, title)
}

// gmaps.Page compile-time assertion.
var _ gmaps.Page = (*pwPage)(nil)
var _ gmaps.PageDiagnosticSource = (*pwPage)(nil)
var _ gmaps.PageDiagnosticObserver = (*pwPage)(nil)
