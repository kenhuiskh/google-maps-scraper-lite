package output

import (
	"encoding/csv"
	"io"

	"github.com/gosom/google-maps-scraper-lite/gmaps"
)

type CSVWriter struct {
	w       *csv.Writer
	headers bool
}

func NewCSVWriter(out io.Writer) *CSVWriter {
	return &CSVWriter{w: csv.NewWriter(out)}
}

func (c *CSVWriter) Write(entry *gmaps.Entry) error {
	if !c.headers {
		if err := c.w.Write(entry.CsvHeaders()); err != nil {
			return err
		}
		c.headers = true
	}
	return c.w.Write(entry.CsvRow())
}

func (c *CSVWriter) Flush() error {
	c.w.Flush()
	return c.w.Error()
}

func (c *CSVWriter) Close() error {
	return c.Flush()
}

var _ Writer = (*CSVWriter)(nil)
