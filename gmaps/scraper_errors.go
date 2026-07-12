package gmaps

import "errors"

// ErrSessionBlocked is returned by Scraper.Run when consecutive place-scrape
// failures indicate the Playwright session has been rate-limited by Google.
var ErrSessionBlocked = errors.New("scraper: session blocked by Google")

// ErrBotBlocked is returned when Google serves a captcha/consent/sorry wall
// or a 429, indicating the session needs the long recovery pause.
var ErrBotBlocked = errors.New("scraper: bot block detected by Google")

// blockThreshold is the number of consecutive failures across all workers
// that triggers an ErrSessionBlocked return.
const blockThreshold = int64(10)
