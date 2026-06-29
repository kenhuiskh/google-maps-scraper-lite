# AGENTS.md

This file provides guidance to Codex (Codex.ai/code) when working with code in this repository.

## Commands

```bash
# Build
go build -o google-maps-scraper-lite .

# Install Playwright browsers (one-time setup required)
go run github.com/playwright-community/playwright-go/cmd/playwright install chromium

# Run all tests
go test ./...

# Run a single test
go test -run Test_EntryFromJSON ./gmaps -v

# Vet and format
go vet ./...
gofmt -s -w .

# Tidy dependencies
go mod tidy
```

## Architecture

The scraper follows a **Feed → Place → Details** pipeline across 4 packages:

```
main.go         CLI flags → Browser init → Scraper → Output writer
browser/        Playwright lifecycle + thread-safe page pool (AcquirePage/ReleasePage)
gmaps/          Core scraping logic
  feed.go       Navigates Google Maps search, scrolls results, returns place URLs
  place.go      Navigates each place URL, extracts APP_INITIALIZATION_STATE JSON via JS injection
  entry.go      Entry struct (28 fields) + all parsing logic for the raw JSON blob
  email.go      Fetches business website HTML and extracts emails via mailto links + regex
  multiple.go   ParseSearchResults for bulk JSON search result format
  scraper.go    Orchestrator: runs feed serially per query, then fans out place scraping with errgroup
output/         Writer interface with CSV and JSON (one-per-line) implementations
```

**Concurrency model:** `gmaps/scraper.go` collects all place URLs from feeds first, then uses `golang.org/x/sync/errgroup` with a semaphore (`-c` flag, default 5) for concurrent place detail scraping. The `browser` package manages a page pool of the same size — workers acquire/release pages around each scrape.

**Data extraction:** Place data comes from Google Maps' `window.APP_INITIALIZATION_STATE` JavaScript variable (deeply nested JSON). `entry.go` navigates this structure using the generic `getNthElementAndCast[T]()` helper. Panic recovery wraps all parsing since the JSON structure can vary.

**Email extraction:** `email.go` fetches the business website (with Google redirect URL normalization), parses HTML for `mailto:` links, and falls back to regex scanning raw HTML. Filtered against social media domains via `IsWebsiteValidForEmail()`.

## Key Details

- Tests live in `gmaps/entry_test.go` with fixtures in `gmaps/testdata/` — these test the JSON parsing logic which is the most fragile part
- `gmaps/entry.go` is the largest file (~820 lines); most bugs related to missing/wrong fields will be here
- The feed scraper handles two cases: multi-result feeds (scrollable list) and single-place direct URLs (detected by URL pattern)
- Google Maps JSON structure has at least 2 known variants; `EntryFromJSON` handles both with fallback logic
