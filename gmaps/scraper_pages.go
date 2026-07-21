package gmaps

import (
	"context"
	"errors"
	"strings"
)

type pageRetirer interface {
	RetirePage(Page)
}

// releaseFeedPage retires a page after any feed failure. In particular, a CDP
// operation timeout means Chromium left a command unanswered; returning that
// page to the pool would make the next query reuse the poisoned tab and hang
// again.
func (s *Scraper) releaseFeedPage(page Page, feedErr error) {
	if page == nil {
		return
	}
	if feedErr != nil {
		if retirer, ok := s.Pool.(pageRetirer); ok {
			retirer.RetirePage(page)
			return
		}
		_ = page.Close()
	}
	s.Pool.ReleasePage(page)
}

func (s *Scraper) releaseScrapePage(page Page, scrapeErr error) {
	if page == nil {
		return
	}
	if shouldRetirePage(scrapeErr) {
		if retirer, ok := s.Pool.(pageRetirer); ok {
			retirer.RetirePage(page)
			return
		}
		_ = page.Close()
	}
	s.Pool.ReleasePage(page)
}

func shouldRetirePage(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, ErrBotBlocked) ||
		errors.Is(err, ErrScrapeDeadline) ||
		errors.Is(err, context.DeadlineExceeded) ||
		strings.Contains(err.Error(), "Page crashed")
}

func (s *Scraper) urlAttemptsExhausted(claimed *ClaimedURL) bool {
	return s.Config.MaxURLAttempts > 0 && claimed != nil && claimed.Attempts >= s.Config.MaxURLAttempts
}
