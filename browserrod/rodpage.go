package browserrod

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/gosom/google-maps-scraper-lite/gmaps"
)

// navTimeout bounds navigation, matching the playwright engine's 60s navigation
// timeout (browser.go configurePage SetDefaultNavigationTimeout).
const navTimeout = 60 * time.Second

// cdpCallTimeout bounds page operations that wait for a Chrome DevTools
// Protocol response. Chromium can occasionally leave Runtime.callFunctionOn
// unanswered while the websocket itself remains open; without a deadline an
// Evaluate call then blocks the entire feed phase until the outer job timeout.
const cdpCallTimeout = 30 * time.Second

// rodPage adapts a *rod.Page to the engine-neutral gmaps.Page interface. All
// rod-specific option structs live here so the gmaps package stays engine-neutral.
//
// closed can be set two ways: our own Close() call, or asynchronously by
// watchCrash's event handler when Chromium's renderer crashes out from under
// us. Because either can happen first, Close's teardown (router.Stop +
// page.Close) is guarded by closeOnce rather than by closed itself — see
// Close's doc comment.
type rodPage struct {
	page      *rod.Page
	router    *rod.HijackRouter // resource-blocking router; stopped on Close
	closed    atomic.Bool
	closeOnce sync.Once
	diagOnce  sync.Once
	diag      *gmaps.PageDiagnosticsState

	// callTimeout and eval are test seams. Production pages use the defaults:
	// cdpCallTimeout and (*rod.Page).Eval.
	callTimeout time.Duration
	eval        func(context.Context, string) (*proto.RuntimeRemoteObject, error)
	// routerStop and pageClose are test seams for teardown calls that can wait
	// on an unhealthy CDP connection. Production uses HijackRouter.Stop and a
	// context-bound rod Page.Close.
	routerStop func() error
	pageClose  func(context.Context) error

	// teardownRuns counts how many times the sync.Once closure inside Close
	// actually executed its body (router.Stop + page.Close). It exists purely
	// as a test seam: it lets rodpage_test.go assert teardown ran exactly once
	// even when closed was pre-set (crash-first) before Close was ever called,
	// which is the scenario the old CAS-guarded Close got wrong.
	teardownRuns atomic.Int32
}

// watchCrash runs for the life of the page on its own event loop (started
// once from newPage) and flips closed when Chromium's renderer crashes
// (OOM, sad-tab, etc). This is distinct from a clean tab close/destroy: rod's
// own internal event loop (Page.initEvents) already reacts to
// proto.TargetTargetDestroyed / proto.TargetDetachedFromTarget by canceling
// the page's session context, which IsClosed's cheap fallback below already
// observes via p.page.GetContext().Err() — so we don't need to subscribe to
// those ourselves. A renderer crash, however, does not necessarily destroy or
// detach the target, so without this explicit subscription IsClosed would
// never see it.
//
// This EachEvent loop shares no goroutine with Goto/Reload's per-navigation
// EachEvent calls (those are short-lived, ending when navigation completes);
// this one lives for the page's lifetime and exits on its own once the page's
// session context is canceled (on crash detection, or on any close), so it
// never leaks.
func (p *rodPage) watchCrash() {
	p.page.EachEvent(func(*proto.InspectorTargetCrashed) bool {
		p.closed.Store(true)
		p.diagnosticState().MarkCrashed()
		return true
	})()
}

// gmaps.Page compile-time assertion.
var _ gmaps.Page = (*rodPage)(nil)

func (p *rodPage) diagnosticState() *gmaps.PageDiagnosticsState {
	p.diagOnce.Do(func() {
		if p.diag == nil {
			p.diag = gmaps.NewPageDiagnosticsState("go-rod")
		}
	})
	return p.diag
}

// Goto navigates to u and returns the main-document HTTP status. rod's Navigate
// returns as soon as the navigation is *initiated* (not committed), so unlike
// playwright's WaitUntil:Commit it does not itself expose the response status.
// We register two ordered event callbacks on a single event loop: the first
// records the main Document response status, the second (returning true) stops
// the loop once the frame commits. Because a single goroutine drains events in
// order, the Document response is always recorded before the frame-navigated
// event ends the wait — no cross-goroutine race on the status.
func (p *rodPage) Goto(u string) (statusResult int, errResult error) {
	done := p.diagnosticState().BeginOperation("goto", u)
	defer func() { done(statusResult, errResult) }()
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
func (p *rodPage) Reload() (statusResult int, errResult error) {
	done := p.diagnosticState().BeginOperation("reload", "")
	defer func() { done(statusResult, errResult) }()
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

func (p *rodPage) Content() (content string, err error) {
	done := p.diagnosticState().BeginOperation("content", "")
	defer func() { done(0, err) }()
	page := p.operationPage()
	defer page.CancelTimeout()
	return page.HTML()
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
func (p *rodPage) Evaluate(js string) (result any, err error) {
	done := p.diagnosticState().BeginOperation("evaluate", "")
	defer func() { done(0, err) }()
	wrapped := "() => { const __r = (" + js + "); return (typeof __r === 'function') ? __r() : __r; }"
	parent := context.Background()
	if p.page != nil {
		parent = p.page.GetContext()
	}
	ctx, cancel := context.WithTimeout(parent, p.operationTimeout())
	defer cancel()
	eval := p.eval
	if eval == nil {
		eval = func(ctx context.Context, js string) (*proto.RuntimeRemoteObject, error) {
			return p.page.Context(ctx).Eval(js)
		}
	}
	obj, err := eval(ctx, wrapped)
	if err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, nil
	}
	return obj.Value.Val(), nil
}

func (p *rodPage) operationPage() *rod.Page {
	return p.page.Timeout(p.operationTimeout())
}

func (p *rodPage) operationTimeout() time.Duration {
	if p.callTimeout > 0 {
		return p.callTimeout
	}
	return cdpCallTimeout
}

// WaitSelector waits up to timeout for selector to be attached to the DOM. rod's
// Element waits for the node to exist/attach, matching the playwright adapter's
// Attached (not Visible) semantics.
func (p *rodPage) WaitSelector(selector string, timeout time.Duration) (err error) {
	done := p.diagnosticState().BeginOperation("wait_selector", "")
	defer func() { done(0, err) }()
	_, err = p.page.Timeout(timeout).Element(selector)
	return err
}

// ClickForce waits for selector to attach then sends a trusted CDP click. rod's
// Click gates on interactability more strictly than playwright's Force:true, so
// we scroll the element into view first to satisfy the viewport check; any
// residual non-interactable error is returned and the call sites already treat
// every error as "skip this selector", so behavior stays equivalent to the
// playwright Force click for the review/hours expansion selectors.
func (p *rodPage) ClickForce(selector string, waitTimeout, clickTimeout time.Duration) (err error) {
	done := p.diagnosticState().BeginOperation("click", "")
	defer func() { done(0, err) }()
	el, err := p.page.Timeout(waitTimeout).Element(selector)
	if err != nil {
		return err
	}
	el = el.Timeout(clickTimeout)
	_ = el.ScrollIntoView()
	return el.Click(proto.InputMouseButtonLeft, 1)
}

func (p *rodPage) URL() string {
	done := p.diagnosticState().BeginOperation("url", "")
	page := p.operationPage()
	defer page.CancelTimeout()
	info, err := page.Info()
	if err != nil {
		done(0, err)
		return ""
	}
	done(0, nil)
	return info.URL
}

func (p *rodPage) Sleep(d time.Duration) {
	time.Sleep(d)
}

// Close is idempotent, guarded by closeOnce rather than the closed atomic:
// watchCrash's handler can flip closed to true before Close is ever called
// (the pool discards a crashed page by calling IsClosed, not Close, so the
// eventual Close on discard/retire must still run teardown even though closed
// is already true). closeOnce guarantees router.Stop + page.Close run exactly
// once regardless of which of Close/watchCrash observes the dead tab first.
// Errors from Stop/page.Close are ignored: both are expected to error on an
// already-dead target (crashed or externally destroyed — e.g. rod's own
// Page.Close waits for a TargetTargetDestroyed that already fired), and no
// caller in this codebase inspects Close's return value.
func (p *rodPage) Close() error {
	done := p.diagnosticState().BeginOperation("close", "")
	defer func() {
		done(0, nil)
		p.diagnosticState().MarkClosed()
	}()
	p.closeOnce.Do(func() {
		p.closed.Store(true)

		// HijackRouter.Stop sends Fetch.disable using the page's original CDP
		// context and has no timeout argument. Run it separately so a missing
		// CDP response cannot trap the caller forever. The page close below uses
		// the same total teardown deadline.
		ctx, cancel := context.WithTimeout(context.Background(), p.operationTimeout())
		defer cancel()
		stopRouter := p.routerStop
		if stopRouter == nil && p.router != nil {
			stopRouter = p.router.Stop
		}
		if stopRouter != nil {
			stopped := make(chan struct{}, 1)
			go func() {
				_ = stopRouter()
				stopped <- struct{}{}
			}()
			select {
			case <-stopped:
			case <-ctx.Done():
			}
		}

		// page is nil only in unit tests that exercise Close's idempotency
		// without a live browser; newPage always sets it in production.
		closePage := p.pageClose
		if closePage == nil && p.page != nil {
			closePage = func(closeCtx context.Context) error {
				return p.page.Context(closeCtx).Close()
			}
		}
		if closePage != nil {
			closed := make(chan struct{}, 1)
			go func() {
				_ = closePage(ctx)
				closed <- struct{}{}
			}()
			select {
			case <-closed:
			case <-ctx.Done():
			}
		}
		p.teardownRuns.Add(1)
	})
	return nil
}

// IsClosed reports true once WE have closed the page, once watchCrash has
// observed a renderer crash, or — the cheap liveness fallback — once rod's
// own internal event loop has canceled the page's session context in
// response to the underlying CDP target being destroyed or detached (see
// watchCrash's doc comment). GetContext().Err() is a simple field read with
// no network round-trip, safe to call on every pool acquire/release.
func (p *rodPage) IsClosed() bool {
	if p.closed.Load() {
		return true
	}
	if p.page == nil {
		return false
	}
	return p.page.GetContext().Err() != nil
}

func (p *rodPage) DiagnosticSnapshot() gmaps.PageDiagnosticSnapshot {
	return p.diagnosticState().Snapshot(p.IsClosed())
}

func (p *rodPage) ObservePageDiagnostics(class string, contentBytes int, title string) {
	p.diagnosticState().ObservePage(class, contentBytes, title)
}

var _ gmaps.PageDiagnosticSource = (*rodPage)(nil)
var _ gmaps.PageDiagnosticObserver = (*rodPage)(nil)
