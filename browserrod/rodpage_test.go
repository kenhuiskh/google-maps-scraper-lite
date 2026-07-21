package browserrod

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/proto"
)

func TestRodPageEvaluateTimesOutUnansweredCDPCall(t *testing.T) {
	p := &rodPage{
		callTimeout: 10 * time.Millisecond,
		eval: func(ctx context.Context, _ string) (*proto.RuntimeRemoteObject, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}

	started := time.Now()
	_, err := p.Evaluate(`() => 1`)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Evaluate error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Evaluate returned after %s, want bounded return", elapsed)
	}
}

// TestRodPage_IsClosed_ReflectsCrashFlag proves the core of the fix: IsClosed
// must report true once the crash-detection path (watchCrash's event handler
// in rodpage.go) has flipped the closed atomic, not only after our own
// Close() has run. watchCrash itself needs a live rod.Page/CDP session to
// exercise end-to-end, so this test isolates the flag semantics it relies on:
// it stores true on the same atomic.Bool the handler uses (p.closed) and
// checks IsClosed observes it immediately, without requiring a browser.
//
// The other half of IsClosed — the p.page.GetContext().Err() liveness
// fallback for a cleanly destroyed/detached target — needs a real *rod.Page
// (GetContext() is not nil-receiver-safe) and is left to the gorod live
// smoke; this test relies on rodPage.page staying nil to exercise the
// closed-flag path alone, which IsClosed's nil guard makes safe.
func TestRodPage_IsClosed_ReflectsCrashFlag(t *testing.T) {
	p := &rodPage{}

	if p.IsClosed() {
		t.Fatal("expected IsClosed() == false before anything closes the page")
	}

	// Simulates watchCrash's handler: p.closed.Store(true) on
	// *proto.InspectorTargetCrashed, independent of our own Close().
	p.closed.Store(true)

	if !p.IsClosed() {
		t.Fatal("expected IsClosed() == true after the crash flag is set")
	}
}

// TestRodPage_IsClosed_NilPageIsSafe guards the nil-page fallback path itself:
// IsClosed must not panic when p.page is nil (true only in this unit test —
// newPage always sets it) and must not report closed just because page is
// unset.
func TestRodPage_IsClosed_NilPageIsSafe(t *testing.T) {
	p := &rodPage{}
	if p.IsClosed() {
		t.Fatal("nil page + unset closed flag should report not-closed")
	}
}

// TestRodPage_Close_RunsTeardownEvenIfCrashFlaggedFirst guards the ordering
// bug the naive fix would reintroduce: if Close's idempotency guard were the
// closed atomic itself (as it was before this fix — a plain
// closed.CompareAndSwap(false, true)), a page that watchCrash already flagged
// as closed would make that CAS fail and skip router.Stop/page.Close
// entirely — leaking the router. closeOnce decouples "is the page reported
// dead" (closed) from "has teardown run" (closeOnce), so Close still attempts
// teardown here even though closed is already true.
//
// p.page and p.router are left nil (Close's nil guards make the actual
// Stop/Close calls safe no-ops), and p.teardownRuns — incremented inside the
// sync.Once closure, right where the real router.Stop/page.Close teardown
// happens — is the observable proof that the closure body ran. Two calls to
// Close() must only run the closure once. This is the test seam: against the
// current sync.Once-guarded Close it passes (teardownRuns == 1); against the
// old CAS-guarded Close it would fail (teardownRuns == 0, because the
// pre-set closed flag makes the CAS fail immediately and the closure body —
// where the counter lives — never runs).
func TestRodPage_Close_RunsTeardownEvenIfCrashFlaggedFirst(t *testing.T) {
	p := &rodPage{}
	p.closed.Store(true) // simulate watchCrash winning the race before Close runs

	if err := p.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := p.Close(); err != nil { // second call (e.g. RetirePage after discard) must be a no-op
		t.Fatalf("second Close() error = %v", err)
	}

	if got := p.teardownRuns.Load(); got != 1 {
		t.Fatalf("teardownRuns = %d, want 1 (teardown must run exactly once even when closed was pre-set)", got)
	}
	if !p.IsClosed() {
		t.Fatal("expected IsClosed() == true throughout")
	}
}
