package browser

import "testing"

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
