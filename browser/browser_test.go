package browser

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gosom/google-maps-scraper-lite/gmaps"
)

// TestAcquirePageCancelledContextReturnsPromptly simulates a saturated pool
// (created == max, no page checked in): AcquirePage must fail fast on a
// cancelled context instead of blocking on the pool channel forever.
func TestAcquirePageCancelledContextReturnsPromptly(t *testing.T) {
	b := &Browser{
		pages:   make(chan gmaps.Page, 1),
		created: 1,
		max:     1,
		uses:    make(map[gmaps.Page]int),
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	type result struct {
		page gmaps.Page
		err  error
	}
	done := make(chan result, 1)
	go func() {
		page, err := b.AcquirePage(ctx)
		done <- result{page, err}
	}()

	select {
	case res := <-done:
		if !errors.Is(res.err, context.Canceled) {
			t.Fatalf("AcquirePage error = %v, want context.Canceled", res.err)
		}
		if res.page != nil {
			t.Fatalf("AcquirePage page = %v, want nil on error", res.page)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("AcquirePage blocked on cancelled context with empty pool")
	}
}

func TestDiagnosticSnapshotIsPassivePoolState(t *testing.T) {
	b := &Browser{
		pages:               make(chan gmaps.Page, 3),
		created:             2,
		max:                 3,
		creating:            1,
		createAt:            time.Now().Add(-time.Second),
		retirements:         4,
		replacements:        3,
		replacementFailures: 1,
		uses:                make(map[gmaps.Page]int),
	}
	snap := b.DiagnosticSnapshot()
	if snap.Engine != "playwright" || snap.Capacity != 3 || snap.Created != 2 || snap.Creating != 1 {
		t.Fatalf("pool snapshot = %#v", snap)
	}
	if snap.Retirements != 4 || snap.Replacements != 3 || snap.ReplacementFailures != 1 {
		t.Fatalf("pool counters = %#v", snap)
	}
	if snap.OldestCreateElapsed < time.Second {
		t.Fatalf("create elapsed = %s, want at least 1s", snap.OldestCreateElapsed)
	}
}

func TestDefaultUserAgents(t *testing.T) {
	if got := len(defaultUserAgents); got < 3 || got > 5 {
		t.Fatalf("default user agent pool length = %d, want 3-5", got)
	}

	seen := make(map[string]bool, len(defaultUserAgents))
	for _, ua := range defaultUserAgents {
		if ua == "" {
			t.Fatal("default user agent pool contains an empty entry")
		}
		if seen[ua] {
			t.Fatalf("default user agent pool contains duplicate entry: %q", ua)
		}
		seen[ua] = true
	}
}

func TestRandomUserAgentFromReturnsPoolMember(t *testing.T) {
	pool := []string{"ua-a", "ua-b", "ua-c"}

	for i := 0; i < 100; i++ {
		got := randomUserAgentFrom(pool)
		if got != "ua-a" && got != "ua-b" && got != "ua-c" {
			t.Fatalf("randomUserAgentFrom() = %q, want member of %v", got, pool)
		}
	}
}

func TestRandomUserAgentFromEmptyPoolUsesFallback(t *testing.T) {
	if got := randomUserAgentFrom(nil); got != fallbackUserAgent {
		t.Fatalf("randomUserAgentFrom(nil) = %q, want fallback user agent", got)
	}
}
