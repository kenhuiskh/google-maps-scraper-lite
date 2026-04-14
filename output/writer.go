package output

import "github.com/gosom/google-maps-scraper-lite/gmaps"

type Writer interface {
	Write(entry *gmaps.Entry) error
	Flush() error
	Close() error
}
