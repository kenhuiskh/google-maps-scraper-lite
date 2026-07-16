//go:build !gorod

package main

import "github.com/gosom/google-maps-scraper-lite/browser"

const engineName = "playwright"

func newBrowserEngine(o browserOptions) (browserEngine, error) {
	return browser.New(browser.Options{
		Concurrency:             o.Concurrency,
		Headless:                o.Headless,
		Lang:                    o.Lang,
		DisableResourceBlocking: o.DisableResourceBlocking,
		BlockedResourceTypes:    o.BlockedResourceTypes,
	})
}
