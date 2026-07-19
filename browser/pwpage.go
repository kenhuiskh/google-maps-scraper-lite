package browser

import (
	"time"

	"github.com/gosom/google-maps-scraper-lite/gmaps"
	"github.com/mxschmitt/playwright-go"
)

// pwPage adapts a playwright.Page to the engine-neutral gmaps.Page interface.
// All playwright option structs live here so the gmaps package stays
// engine-neutral.
type pwPage struct {
	page playwright.Page
}

func (p *pwPage) Goto(u string) (int, error) {
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

func (p *pwPage) Reload() (int, error) {
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

func (p *pwPage) Content() (string, error) {
	return p.page.Content()
}

func (p *pwPage) Evaluate(js string) (any, error) {
	return p.page.Evaluate(js)
}

// WaitSelector maps to Locator.WaitFor(Attached) rather than the "visible"
// default that playwright's own WaitForSelector uses. This is a deliberate,
// documented semantic nuance: the feed div is visible whenever it's attached,
// so this keeps a single wait primitive for both engines without changing
// observed behavior.
func (p *pwPage) WaitSelector(selector string, timeout time.Duration) error {
	return p.page.Locator(selector).First().WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateAttached,
		Timeout: playwright.Float(float64(timeout.Milliseconds())),
	})
}

func (p *pwPage) ClickForce(selector string, waitTimeout, clickTimeout time.Duration) error {
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
	return p.page.URL()
}

func (p *pwPage) Sleep(d time.Duration) {
	p.page.WaitForTimeout(float64(d.Milliseconds()))
}

func (p *pwPage) Close() error {
	return p.page.Close()
}

func (p *pwPage) IsClosed() bool {
	return p.page.IsClosed()
}

// gmaps.Page compile-time assertion.
var _ gmaps.Page = (*pwPage)(nil)
