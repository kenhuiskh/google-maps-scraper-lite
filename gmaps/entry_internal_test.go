package gmaps

import (
	"reflect"
	"testing"
)

func TestFindReviewTimestampLocatesNestedDate(t *testing.T) {
	nested := []any{
		"noise",
		[]any{float64(1), float64(2)},
		[]any{
			[]any{float64(2025), float64(12), float64(8), float64(14), float64(3)},
		},
	}
	got := findReviewTimestamp(nested, 0)
	if len(got) < 3 || got[0] != float64(2025) || got[1] != float64(12) || got[2] != float64(8) {
		t.Fatalf("findReviewTimestamp() = %#v, want the 2025-12-8 triple", got)
	}
}

func TestFindReviewTimestampRejectsNonDates(t *testing.T) {
	for name, node := range map[string]any{
		"too short":       []any{float64(2025), float64(12)},
		"month overflow":  []any{[]any{float64(2025), float64(13), float64(8)}},
		"day overflow":    []any{[]any{float64(2025), float64(12), float64(32)}},
		"year too early":  []any{[]any{float64(1999), float64(12), float64(8)}},
		"non numeric":     []any{[]any{"2025", "12", "8"}},
		"fractional year": []any{[]any{float64(2025.5), float64(12), float64(8)}},
		"not an array":    "2025-12-08",
	} {
		t.Run(name, func(t *testing.T) {
			if got := findReviewTimestamp(node, 0); got != nil {
				t.Fatalf("findReviewTimestamp() = %#v, want nil", got)
			}
		})
	}
}

func TestFindReviewTimestampStopsAtDepthLimit(t *testing.T) {
	node := any([]any{float64(2025), float64(12), float64(8)})
	for range maxTimestampSearchDepth + 2 {
		node = []any{node}
	}
	if got := findReviewTimestamp(node, 0); got != nil {
		t.Fatalf("findReviewTimestamp() = %#v past the depth limit, want nil", got)
	}
}

func TestEpochToDateParts(t *testing.T) {
	// Micro-, milli-, and second-precision forms of 2021-06-25T21:54:56Z.
	for name, value := range map[string]float64{
		"microseconds": 1624658096059139,
		"milliseconds": 1624658096059,
		"seconds":      1624658096,
	} {
		t.Run(name, func(t *testing.T) {
			got := epochToDateParts(value)
			want := []any{float64(2021), float64(6), float64(25)}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("epochToDateParts(%v) = %#v, want %#v", value, got, want)
			}
		})
	}

	for name, value := range map[string]float64{
		"zero":           0,
		"negative":       -1624658096,
		"fractional":     1624658096.5,
		"too small":      12345,
		"far past epoch": 1,
	} {
		t.Run(name, func(t *testing.T) {
			if got := epochToDateParts(value); got != nil {
				t.Fatalf("epochToDateParts(%v) = %#v, want nil", value, got)
			}
		})
	}
}

func TestFindReviewEpochDateTakesFirstInDocumentOrder(t *testing.T) {
	// Creation timestamp first, a later edit timestamp after it.
	el := []any{
		"author",
		[]any{[]any{float64(1624658096059139)}},
		[]any{float64(1624801604000000)},
	}
	got := findReviewEpochDate(el, 0)
	want := []any{float64(2021), float64(6), float64(25)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("findReviewEpochDate() = %#v, want %#v (creation, not edit)", got, want)
	}
}
