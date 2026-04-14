package gmaps

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/mcnijman/go-emailaddress"
)

var httpClient = &http.Client{Timeout: 10 * time.Second}

// ExtractEmails fetches the given URL and returns a deduplicated list of email
// addresses found in the page (via mailto links and regex scan of the body).
func ExtractEmails(ctx context.Context, websiteURL string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, websiteURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	emails := make(map[string]struct{})

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err == nil {
		for _, e := range docEmailExtractor(doc) {
			emails[e] = struct{}{}
		}
	}

	for _, e := range regexEmailExtractor(body) {
		emails[e] = struct{}{}
	}

	result := make([]string, 0, len(emails))
	for e := range emails {
		result = append(result, e)
	}

	return result, nil
}

// docEmailExtractor extracts email addresses from mailto: links in a parsed HTML document.
func docEmailExtractor(doc *goquery.Document) []string {
	seen := map[string]bool{}

	var emails []string

	doc.Find("a[href^='mailto:']").Each(func(_ int, s *goquery.Selection) {
		mailto, exists := s.Attr("href")
		if exists {
			value := strings.TrimPrefix(mailto, "mailto:")
			if email, err := getValidEmail(value); err == nil {
				if !seen[email] {
					emails = append(emails, email)
					seen[email] = true
				}
			}
		}
	})

	return emails
}

// regexEmailExtractor finds email addresses in raw HTML bytes using regex scanning.
func regexEmailExtractor(body []byte) []string {
	seen := map[string]bool{}

	var emails []string

	addresses := emailaddress.Find(body, false)
	for i := range addresses {
		if !seen[addresses[i].String()] {
			emails = append(emails, addresses[i].String())
			seen[addresses[i].String()] = true
		}
	}

	return emails
}

// getValidEmail parses and validates a single email address string.
func getValidEmail(s string) (string, error) {
	email, err := emailaddress.Parse(strings.TrimSpace(s))
	if err != nil {
		return "", err
	}

	return email.String(), nil
}

// normalizeGoogleURL extracts the actual target URL from Google redirect URLs.
// Google Maps sometimes returns URLs like "/url?q=http://example.com/&opi=..."
// for external website links.
func normalizeGoogleURL(raw string) string {
	if raw == "" {
		return raw
	}

	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}

	if q := u.Query().Get("q"); q != "" {
		return q
	}

	return raw
}
