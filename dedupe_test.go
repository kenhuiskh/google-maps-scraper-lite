package main

import (
	"testing"

	"github.com/gosom/google-maps-scraper-lite/gmaps"
)

func TestPlaceDeduperMatchesAnyGoogleID(t *testing.T) {
	dedupe := newPlaceDeduper()

	if dedupe.Seen(&gmaps.Entry{Cid: "cid-1", PlaceID: "place-1", DataID: "data-1"}) {
		t.Fatal("first entry should not be seen")
	}

	tests := []struct {
		name  string
		entry *gmaps.Entry
	}{
		{
			name:  "same cid",
			entry: &gmaps.Entry{Cid: "cid-1", PlaceID: "place-2", DataID: "data-2"},
		},
		{
			name:  "same place_id",
			entry: &gmaps.Entry{Cid: "cid-3", PlaceID: "place-1", DataID: "data-3"},
		},
		{
			name:  "same data_id",
			entry: &gmaps.Entry{Cid: "cid-4", PlaceID: "place-4", DataID: "data-1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !dedupe.Seen(tt.entry) {
				t.Fatal("entry should be treated as duplicate")
			}
		})
	}
}

func TestPlaceDeduperIgnoresEmptyGoogleIDs(t *testing.T) {
	dedupe := newPlaceDeduper()

	if dedupe.Seen(&gmaps.Entry{}) {
		t.Fatal("empty identifiers should not be marked duplicate")
	}
	if dedupe.Seen(&gmaps.Entry{}) {
		t.Fatal("empty identifiers should not be remembered")
	}
}
