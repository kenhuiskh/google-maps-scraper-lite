package browserrod

import (
	"sync/atomic"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/gosom/google-maps-scraper-lite/gmaps"
)

// navTimeout bounds navigation, matching the playwright engine's 60s navigation
// timeout (browser.go configurePage SetDefaultNavigationTimeout).
const navTimeout = 60 * time.Second

// rodPage adapts a *rod.Page to the engine-neutral gmaps.Page interface. All
// rod-specific option structs live here so the gmaps package stays engine-neutral.
type rodPage struct {
	page   *rod.Page
	router *rod.HijackRouter // resource-blocking router; stopped on Close
	closed atomic.Bool
}

// gmaps.Page compile-time assertion.
var _ gmaps.Page = (*rodPage)(nil)

// Goto navigates to u and returns the main-document HTTP status. rod's Navigate
// returns as soon as the navigation is *initiated* (not committed), so unlike
// playwright's WaitUntil:Commit it does not itself expose the response status.
// We register two ordered event callbacks on a single event loop: the first
// records the main Document response status, the second (returning true) stops
// the loop once the frame commits. Because a single goroutine drains events in
// order, the Document response is always recorded before the frame-navigated
// event ends the wait — no cross-goroutine race on the status.
func (p *rodPage) Goto(u string) (int, error) {
	var status int32
	page := p.page.Timeout(navTimeout)
	frameID := page.FrameID
	wait := page.EachEvent(
		func(e *proto.NetworkResponseReceived) {
			if e.Type == proto.NetworkResourceTypeDocument && e.Response != nil {
				atomic.StoreInt32(&status, int32(e.Response.Status))
			}
		},
		func(e *proto.PageFrameNavigated) bool {
			return e.Frame != nil && e.Frame.ID == frameID
		},
	)
	if err := page.Navigate(u); err != nil {
		return 0, err
	}
	wait()
	return int(atomic.LoadInt32(&status)), nil
}

// Reload reloads the current page and returns the main-document status, using
// the same ordered-event capture as Goto so the Document response status is
// recorded before the wait ends.
func (p *rodPage) Reload() (int, error) {
	var status int32
	page := p.page.Timeout(navTimeout)
	frameID := page.FrameID
	wait := page.EachEvent(
		func(e *proto.NetworkResponseReceived) {
			if e.Type == proto.NetworkResourceTypeDocument && e.Response != nil {
				atomic.StoreInt32(&status, int32(e.Response.Status))
			}
		},
		func(e *proto.PageFrameNavigated) bool {
			return e.Frame != nil && e.Frame.ID == frameID
		},
	)
	// location.reload() (via a trusted user gesture) mirrors rod's own Page.Reload
	// while letting us keep our own status/commit event loop.
	if _, err := page.Eval(`() => location.reload()`); err != nil {
		return 0, err
	}
	wait()
	return int(atomic.LoadInt32(&status)), nil
}

func (p *rodPage) Content() (string, error) {
	return p.page.HTML()
}

// Evaluate runs a gmaps JS blob and returns its result decoded to native Go
// values. The blobs are a MIX of shapes: some are (async) arrow functions the
// playwright engine CALLS (placeJS scroller, feed scroller, cookie-reject);
// others are self-invoked IIFE expressions whose value is used directly
// (reviewTagsJS, extractDOMReviewsJS, scrollReviewsFeedJS, ...). rod's Eval
// treats its argument as a function to apply, which would mishandle the IIFE
// blobs. A universal shim evaluates the blob as an expression and only calls it
// when it turns out to be a function, matching playwright for BOTH shapes
// without editing any blob. rod's Page.Eval awaits returned promises and returns
// by value, so the async arrow blobs (which resolve after a setTimeout) settle
// and gson decodes to string / float64 / bool / []interface{} /
// map[string]interface{} / nil — the same shapes the playwright engine yields,
// so the existing json.Marshal round-trips are unchanged.
func (p *rodPage) Evaluate(js string) (any, error) {
	wrapped := "() => { const __r = (" + js + "); return (typeof __r === 'function') ? __r() : __r; }"
	obj, err := p.page.Eval(wrapped)
	if err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, nil
	}
	return obj.Value.Val(), nil
}

// WaitSelector waits up to timeout for selector to be attached to the DOM. rod's
// Element waits for the node to exist/attach, matching the playwright adapter's
// Attached (not Visible) semantics.
func (p *rodPage) WaitSelector(selector string, timeout time.Duration) error {
	_, err := p.page.Timeout(timeout).Element(selector)
	return err
}

// ClickForce waits for selector to attach then sends a trusted CDP click. rod's
// Click gates on interactability more strictly than playwright's Force:true, so
// we scroll the element into view first to satisfy the viewport check; any
// residual non-interactable error is returned and the call sites already treat
// every error as "skip this selector", so behavior stays equivalent to the
// playwright Force click for the review/hours expansion selectors.
func (p *rodPage) ClickForce(selector string, waitTimeout, clickTimeout time.Duration) error {
	el, err := p.page.Timeout(waitTimeout).Element(selector)
	if err != nil {
		return err
	}
	el = el.Timeout(clickTimeout)
	_ = el.ScrollIntoView()
	return el.Click(proto.InputMouseButtonLeft, 1)
}

func (p *rodPage) URL() string {
	info, err := p.page.Info()
	if err != nil {
		return ""
	}
	return info.URL
}

func (p *rodPage) Sleep(d time.Duration) {
	time.Sleep(d)
}

// Close is idempotent. It stops the resource-blocking router (if any) and closes
// the tab. IsClosed reflects the closed flag since rod has no IsClosed of its own
// and the pool relies on it for dead-page detection.
func (p *rodPage) Close() error {
	if !p.closed.CompareAndSwap(false, true) {
		return nil
	}
	if p.router != nil {
		_ = p.router.Stop()
	}
	return p.page.Close()
}

func (p *rodPage) IsClosed() bool {
	return p.closed.Load()
}
