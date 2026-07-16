package gmaps

import "time"

// Page is the minimal browser-page surface the feed/place scrapers need. Both the
// playwright-backed adapter (browser package) and the rod-backed adapter implement it,
// keeping the scraper orchestration engine-neutral.
type Page interface {
	// Goto navigates to url, waiting until the navigation is committed, and returns
	// the HTTP status of the main response (0 when no response/status is available).
	Goto(url string) (status int, err error)
	// Reload reloads the current page (commit wait) and returns the main-response status.
	Reload() (status int, err error)
	// Content returns the current full serialized HTML of the page.
	Content() (string, error)
	// Evaluate runs a JavaScript expression (no arguments) and returns its result,
	// decoded to Go values (string / float64 / []any / map[string]any / nil).
	Evaluate(js string) (any, error)
	// WaitSelector waits up to timeout for an element matching selector to be attached
	// to the DOM. Returns a non-nil error if it never attaches within timeout.
	WaitSelector(selector string, timeout time.Duration) error
	// ClickForce waits up to waitTimeout for selector to attach, then sends a trusted
	// (isTrusted=true) click that bypasses actionability/visibility checks, bounded by
	// clickTimeout. Returns a non-nil error if either the wait or the click fails.
	ClickForce(selector string, waitTimeout, clickTimeout time.Duration) error
	// URL returns the page's current URL.
	URL() string
	// Sleep pauses for d (the page-driver's fixed-wait primitive).
	Sleep(d time.Duration)
	// Close closes the page; IsClosed reports whether it has been closed.
	Close() error
	IsClosed() bool
}
