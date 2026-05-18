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

// sanitizeCSVCell prefixes cells that start with characters interpreted as
// formulas by spreadsheet software (=, +, -, @, tab, CR) with a leading
// single-quote. Prevents CSV-injection when scraped fields like business
// titles begin with such characters.
func sanitizeCSVCell(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	}
	return s
}

func sanitizeCSVRow(row []string) []string {
	out := make([]string, len(row))
	for i, cell := range row {
		out[i] = sanitizeCSVCell(cell)
	}
	return out
}

func (c *CSVWriter) Write(entry *gmaps.Entry) error {
	if !c.headers {
		if err := c.w.Write(entry.CsvHeaders()); err != nil {
			return err
		}
		c.headers = true
	}
	return c.w.Write(sanitizeCSVRow(entry.CsvRow()))
}

func (c *CSVWriter) Flush() error {
	c.w.Flush()
	return c.w.Error()
}

func (c *CSVWriter) Close() error {
	return c.Flush()
}

var _ Writer = (*CSVWriter)(nil)
