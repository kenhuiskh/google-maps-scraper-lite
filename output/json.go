package output

import (
	"encoding/json"
	"io"

	"github.com/gosom/google-maps-scraper-lite/gmaps"
)

type JSONWriter struct {
	enc *json.Encoder
}

func NewJSONWriter(out io.Writer) *JSONWriter {
	return &JSONWriter{enc: json.NewEncoder(out)}
}

func (j *JSONWriter) Write(entry *gmaps.Entry) error {
	return j.enc.Encode(entry)
}

func (j *JSONWriter) Flush() error { return nil }
func (j *JSONWriter) Close() error { return nil }

var _ Writer = (*JSONWriter)(nil)
