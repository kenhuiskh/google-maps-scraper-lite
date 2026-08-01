package browserrod

import (
	"strings"
	"testing"
	"time"

	"github.com/gosom/google-maps-scraper-lite/gmaps"
)

func TestDiagnosticSnapshotIsPassivePoolState(t *testing.T) {
	b := &Browser{
		pages:               make(chan gmaps.Page, 2),
		created:             2,
		max:                 2,
		creating:            1,
		createAt:            time.Now().Add(-time.Second),
		retirements:         5,
		replacements:        4,
		replacementFailures: 2,
		uses:                make(map[gmaps.Page]int),
	}
	snap := b.DiagnosticSnapshot()
	if snap.Engine != "go-rod" || snap.Capacity != 2 || snap.Created != 2 || snap.Creating != 1 {
		t.Fatalf("pool snapshot = %#v", snap)
	}
	if snap.Retirements != 5 || snap.Replacements != 4 || snap.ReplacementFailures != 2 {
		t.Fatalf("pool counters = %#v", snap)
	}
	if snap.OldestCreateElapsed < time.Second {
		t.Fatalf("create elapsed = %s, want at least 1s", snap.OldestCreateElapsed)
	}
}

func TestRandomUserAgentFromEmptyReturnsFallback(t *testing.T) {
	if got := randomUserAgentFrom(nil); got != fallbackUserAgent {
		t.Fatalf("empty pool: got %q, want fallback", got)
	}
	if got := randomUserAgentFrom([]string{}); got != fallbackUserAgent {
		t.Fatalf("empty slice: got %q, want fallback", got)
	}
}

func TestRandomUserAgentFromPicksFromPool(t *testing.T) {
	pool := []string{"a", "b", "c"}
	seen := map[string]struct{}{}
	for i := 0; i < 200; i++ {
		ua := randomUserAgentFrom(pool)
		if ua != "a" && ua != "b" && ua != "c" {
			t.Fatalf("returned UA not in pool: %q", ua)
		}
		seen[ua] = struct{}{}
	}
	if len(seen) == 0 {
		t.Fatal("expected at least one UA")
	}
}

func TestRandomUserAgentUsesDefaultPool(t *testing.T) {
	ua := randomUserAgent()
	for _, cand := range defaultUserAgents {
		if ua == cand {
			return
		}
	}
	t.Fatalf("randomUserAgent returned %q not in defaultUserAgents", ua)
}

func TestBuildBlockedSetDefaults(t *testing.T) {
	set := buildBlockedSet(nil)
	for _, typ := range []string{"image", "media", "font"} {
		if _, ok := set[typ]; !ok {
			t.Fatalf("default set missing %q", typ)
		}
	}
	if _, ok := set["document"]; ok {
		t.Fatal("default set should not block document")
	}
	if len(set) != 3 {
		t.Fatalf("default set size = %d, want 3", len(set))
	}
}

// TestBuildBlockedSetLowercasesForRodCasing guards the key correctness detail:
// rod reports capitalized resource types (Image/Media/Font) while the config and
// playwright engine use lowercase. The set is lowercased so a strings.ToLower
// comparison of rod's type matches, and mixed-case config is normalized too.
func TestBuildBlockedSetLowercasesForRodCasing(t *testing.T) {
	set := buildBlockedSet([]string{"Image", "MEDIA", "stylesheet"})
	for _, rodType := range []string{"Image", "Media", "Stylesheet"} {
		if _, ok := set[strings.ToLower(rodType)]; !ok {
			t.Fatalf("rod type %q not blocked after lowercasing", rodType)
		}
	}
	if _, ok := set[strings.ToLower("Font")]; ok {
		t.Fatal("Font should not be blocked with this config")
	}
}
