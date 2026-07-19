//go:build gorod

package main

import "github.com/gosom/google-maps-scraper-lite/browserrod"

const engineName = "go-rod"

func newBrowserEngine(o browserOptions) (browserEngine, error) {
	return browserrod.New(browserrod.Options{
		Concurrency:             o.Concurrency,
		Headless:                o.Headless,
		Lang:                    o.Lang,
		DisableResourceBlocking: o.DisableResourceBlocking,
		BlockedResourceTypes:    o.BlockedResourceTypes,
	})
}
