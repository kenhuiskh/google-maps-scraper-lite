package main

import (
	"context"

	"github.com/gosom/google-maps-scraper-lite/gmaps"
)

// browserEngine is the browser backend the scraper drives; a playwright-backed
// and a go-rod-backed implementation are selected at build time (see engine_*.go).
type browserEngine interface {
	AcquirePage(context.Context) (gmaps.Page, error)
	ReleasePage(gmaps.Page)
	RetirePage(gmaps.Page)
	Close() error
}

// browserOptions is the engine-neutral config mapped to the selected backend's Options.
type browserOptions struct {
	Concurrency             int
	Headless                bool
	Lang                    string
	DisableResourceBlocking bool
	BlockedResourceTypes    []string
}
