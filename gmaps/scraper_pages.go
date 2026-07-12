package gmaps

import (
	"errors"
	"strings"

	"github.com/mxschmitt/playwright-go"
)

type pageRetirer interface {
	RetirePage(playwright.Page)
}

func (s *Scraper) releaseScrapePage(page playwright.Page, scrapeErr error) {
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
		strings.Contains(err.Error(), "Page crashed")
}

func (s *Scraper) urlAttemptsExhausted(claimed *ClaimedURL) bool {
	return s.Config.MaxURLAttempts > 0 && claimed != nil && claimed.Attempts >= s.Config.MaxURLAttempts
}
