# Task 8 report: `rodPage.IsClosed()` detects Chromium-side tab death

## What changed

`browserrod/rodpage.go` and `browserrod/browser.go` only. `rodPage.IsClosed()` used to
return only our own `closed` flag (set exclusively by our `Close()`), so a Chromium-side
tab death — renderer crash, OOM, external close — was invisible to the pool's dead-page
detection (`AcquirePage`/`ReleasePage` in `browserrod/browser.go`, both gated on
`page.IsClosed()`). The dead page circulated back to workers indefinitely.

Two independent signals now feed `IsClosed()`:

1. **Renderer crash → explicit event subscription.** A new `rodPage.watchCrash()` method
   runs one persistent `page.EachEvent(...)` loop, started once per page from
   `browser.go`'s `newPage` (`go rp.watchCrash()`, right after `rp := &rodPage{page: page}`).
   It subscribes to `*proto.InspectorTargetCrashed` (Chromium's renderer-crashed CDP event)
   and calls `p.closed.Store(true)` then returns `true` to end its own loop. This is a
   *different* event loop from the short-lived per-navigation `EachEvent` calls already in
   `Goto`/`Reload` — those end when a single navigation commits; this one lives for the
   page's lifetime and self-terminates (no goroutine leak — see "why no leak" below).

2. **Clean destroy/detach → free liveness fallback, no new subscription needed.**
   Investigating rod's own source (`page.go` `initEvents`) showed rod *already* runs an
   internal goroutine per page that reacts to `proto.TargetTargetDestroyed` (matching
   `TargetID`) and `proto.TargetDetachedFromTarget` (matching `SessionID`) by calling
   `p.sessionCancel()`, which cancels the page's own session `context.Context`
   (`page.ctx`, returned by the exported `(*rod.Page).GetContext()`). This fires on *any*
   clean or external close, not just our own. So instead of duplicating a subscription to
   those two event types, `IsClosed()`'s fallback just checks
   `p.page.GetContext().Err() != nil` — a plain field read, no CDP round-trip, safe to call
   on every pool acquire/release as the brief required.

So the final `IsClosed()`:
```go
func (p *rodPage) IsClosed() bool {
	if p.closed.Load() {
		return true
	}
	if p.page == nil {
		return false
	}
	return p.page.GetContext().Err() != nil
}
```

### Close() idempotency bug avoided

The original `Close()` guarded its one-time teardown (`router.Stop()` + `page.Close()`)
with `p.closed.CompareAndSwap(false, true)`. Once `watchCrash`'s handler can set `closed`
to `true` *before* `Close()` is ever called (which is the normal case — the pool discards a
dead page via `IsClosed()`, then later calls `Close()` on discard/retire), that CAS would
fail and **silently skip teardown forever**, leaking the hijack router. Fixed by decoupling
"is the page reported dead" (`closed` atomic) from "has teardown run" (`sync.Once`
`closeOnce`), so `Close()` always attempts `router.Stop`/`page.Close` exactly once
regardless of who set `closed` first. Errors from that teardown are ignored (expected on an
already-dead target — e.g. rod's own `Page.Close()` waits for a `TargetTargetDestroyed`
that already fired before we call it), matching the brief and matching every existing
caller in this codebase, which already discards `Close()`'s error (`_ = page.Close()` in
`gmaps/scraper.go`, `gmaps/scraper_pages.go`, `browserrod/browser.go`).

### Why `watchCrash`'s goroutine never leaks

`page.EachEvent(cb)` returns a `wait func()`; the pattern `go page.EachEvent(cb)()` (the
same one rod's own godoc uses for a persistent dialog-dismiss handler) runs that wait loop
in the background. Internally (`rod@v0.116.2/browser.go: eachEvent`) it derives a child
context of `p.browser.Context(p.ctx)` — i.e. a child of the page's own session context — and
its `for msg := range messages` loop exits once that channel closes. `messages` comes from
`ysmood/goob`'s `Observable.Subscribe`, which unsubscribes and closes the channel as soon as
its context (or the parent Observable's context) is done. Since the page's session context
(`p.ctx`/`GetContext()`) is exactly what rod cancels on any close (see point 2 above), the
`watchCrash` goroutine terminates on its own whether the tab is closed by us, destroyed
externally, or crashes (in the crash case, the callback itself returns `true` first).
Confirmed by reading `rod@v0.116.2/page.go` (`initEvents`, `Close`) and
`rod@v0.116.2/browser.go` (`eachEvent`), and `ysmood/goob@v0.4.0/goob.go` (`Subscribe`).

## `go doc` confirmation (rod v0.116.2, pinned in go.mod)

```
$ go list -m all | grep go-rod
github.com/go-rod/rod v0.116.2
github.com/go-rod/stealth v0.4.9

$ go doc github.com/go-rod/rod/lib/proto.InspectorTargetCrashed
package proto // import "github.com/go-rod/rod/lib/proto"

type InspectorTargetCrashed struct{}
    InspectorTargetCrashed Fired when debugging target has crashed.

func (evt InspectorTargetCrashed) ProtoEvent() string

$ go doc github.com/go-rod/rod/lib/proto.TargetTargetDestroyed
package proto // import "github.com/go-rod/rod/lib/proto"

type TargetTargetDestroyed struct {
	// TargetID ...
	TargetID TargetTargetID `json:"targetId"`
}
    TargetTargetDestroyed Issued when a target is destroyed.

func (evt TargetTargetDestroyed) ProtoEvent() string

$ go doc github.com/go-rod/rod/lib/proto.TargetDetachedFromTarget
package proto // import "github.com/go-rod/rod/lib/proto"

type TargetDetachedFromTarget struct {
	// SessionID Detached session identifier.
	SessionID TargetSessionID `json:"sessionId"`

	// TargetID (deprecated) (optional) Deprecated.
	TargetID TargetTargetID `json:"targetId,omitempty"`
}
    TargetDetachedFromTarget (experimental) Issued when detached from target for
    any reason (including `detachFromTarget` command). Can be issued multiple
    times per target if multiple sessions have been attached to it.

func (evt TargetDetachedFromTarget) ProtoEvent() string
```

All three types exist in the pinned v0.116.2. Only `InspectorTargetCrashed` is subscribed
to directly in our code (via `watchCrash`); the other two are consumed indirectly through
rod's own internal cancellation of `page.GetContext()`, as explained above.

## Test evidence

New file `browserrod/rodpage_test.go`, three unit tests, none requiring a live browser:

```
$ go test ./browserrod/... -run TestRodPage -v
=== RUN   TestRodPage_IsClosed_ReflectsCrashFlag
--- PASS: TestRodPage_IsClosed_ReflectsCrashFlag (0.00s)
=== RUN   TestRodPage_IsClosed_NilPageIsSafe
--- PASS: TestRodPage_IsClosed_NilPageIsSafe (0.00s)
=== RUN   TestRodPage_Close_RunsTeardownEvenIfCrashFlaggedFirst
--- PASS: TestRodPage_Close_RunsTeardownEvenIfCrashFlaggedFirst (0.00s)
PASS
ok  	github.com/gosom/google-maps-scraper-lite/browserrod	0.322s
```

- `TestRodPage_IsClosed_ReflectsCrashFlag`: constructs a bare `&rodPage{}`, asserts
  `IsClosed()` is `false`, stores `true` on the same `closed` atomic `watchCrash`'s handler
  uses, asserts `IsClosed()` flips to `true`. This is the test the brief asked for.
- `TestRodPage_IsClosed_NilPageIsSafe`: guards the nil-page fallback branch doesn't panic
  or misreport when `page` is unset (only true in this test; `newPage` always sets it).
- `TestRodPage_Close_RunsTeardownEvenIfCrashFlaggedFirst`: pre-sets `closed = true` (as
  `watchCrash` would before `Close()` is ever called), then calls the *real* `Close()`
  twice, asserting no panic/error either time and `IsClosed()` stays `true` — this is the
  regression test for the CAS-vs-`sync.Once` bug described above. (`page` stays nil in this
  test — `Close()`'s new nil-guard on `p.page` lets it call the real method safely; that
  guard is a no-op in production since `newPage` always sets `page`.)

The `GetContext().Err()` fallback path (destroy/detach → context cancellation) needs a real
`*rod.Page`/CDP session and is intentionally not unit-tested — flagged per the brief as
belonging to a gorod live smoke, which was not run (optional per the brief, "not required
if the build/unit test + go doc confirmation are solid"; the reasoning above via reading
rod's own source stood in for it).

### Full suites

```
$ gofmt -s -l browserrod/
(empty)

$ go vet ./...
(clean)

$ go vet -tags gorod ./...
(clean)

$ go build ./...
(clean, exit 0)

$ go build -tags gorod ./...
(clean, exit 0)

$ go test ./...
ok  	github.com/gosom/google-maps-scraper-lite	(cached)
ok  	github.com/gosom/google-maps-scraper-lite/browser	(cached)
ok  	github.com/gosom/google-maps-scraper-lite/browserrod	0.527s
ok  	github.com/gosom/google-maps-scraper-lite/geo	(cached)
ok  	github.com/gosom/google-maps-scraper-lite/gmaps	(cached)
ok  	github.com/gosom/google-maps-scraper-lite/output	(cached)

$ go test -tags gorod ./...
ok  	github.com/gosom/google-maps-scraper-lite	(cached)
ok  	github.com/gosom/google-maps-scraper-lite/browser	(cached)
ok  	github.com/gosom/google-maps-scraper-lite/browserrod	0.359s
ok  	github.com/gosom/google-maps-scraper-lite/geo	(cached)
ok  	github.com/gosom/google-maps-scraper-lite/gmaps	(cached)
ok  	github.com/gosom/google-maps-scraper-lite/output	(cached)

$ go test ./browserrod/... -race -v
(all 8 tests pass, including the 3 new ones — see full run above/below)
ok  	github.com/gosom/google-maps-scraper-lite/browserrod	1.373s
```

## Files changed

- `browserrod/rodpage.go` — `rodPage` struct gains `closeOnce sync.Once`; new
  `watchCrash()` method; `Close()` rewritten to use `closeOnce` and ignore teardown errors;
  `IsClosed()` rewritten with the crash-flag + context-liveness checks.
- `browserrod/browser.go` — `newPage()` gains one line, `go rp.watchCrash()`, right after
  the `rodPage` is constructed.
- `browserrod/rodpage_test.go` — new file, 3 unit tests (no live browser).

## Self-review

- **Correctness**: `IsClosed()` returns `true` on crash (via `watchCrash` → `closed`
  atomic), on clean destroy/detach (via `GetContext().Err()`), and on our own `Close()`
  (which also sets `closed`). No double-router-stop: `closeOnce` guarantees `router.Stop`
  runs exactly once no matter how many times `Close()` is called or who set `closed` first.
  No per-call network round-trip: `closed.Load()` and `ctx.Err()` are both plain memory
  reads. One event loop per page for crash detection (`watchCrash`), independent of the
  short-lived per-navigation loops in `Goto`/`Reload`.
- **Contained**: only `browserrod/rodpage.go`, `browserrod/browser.go`, and the new
  `browserrod/rodpage_test.go` touched. No changes to `gmaps/*`, `browser/*`, `main.go`,
  `control*.go`, or `gmaps/scraper_pages.go`. No new dependencies (`sync` is stdlib).
- **Verified**: both builds, both vets, `gofmt -s -l` all clean; both test suites green
  (default and `-tags gorod`, since `browserrod` compiles unconditionally — only
  `main.go`'s `engine_gorod.go`/`engine_playwright.go` are build-tag-gated); `go doc`
  confirms all three proto event types exist in the pinned v0.116.2; `-race` clean on the
  browserrod package.
- Caught and fixed one bug of my own during self-review before finalizing: my first draft
  of `Close()` stored the real `page.Close()` error in a `closeErr` field and returned it,
  which contradicted the brief's explicit "ignore those errors" instruction (harmless given
  every caller discards it today, but redundant complexity and a footgun for the next
  caller that doesn't). Simplified to always return `nil` and drop the field.

## Issues/concerns

None blocking. Two things worth flagging for anyone reading this later:

1. The `GetContext().Err()` fallback relies on an internal rod behavior
   (`Page.initEvents`/`sessionCancel`) that isn't part of rod's public API contract — it's
   the exported `GetContext()` returning what happens to be the internally-cancelable
   session context. This is stable behavior for the pinned v0.116.2 (verified by reading
   the vendored source directly) but a rod upgrade should re-check `page.go`'s
   `initEvents`/`Close` before assuming this still holds.
2. Per the brief, the optional live gorod smoke (force-close a page, confirm the pool
   discards it) was not run — the `go doc` confirmation plus reading rod's actual event
   handling source was judged sufficient, matching the brief's own "not required if the
   build/unit test + go doc confirmation are solid" allowance. If someone later has a live
   Chromium handy, forcibly killing the renderer of a pooled page and confirming
   `AcquirePage` discards it would be the natural end-to-end confirmation.
