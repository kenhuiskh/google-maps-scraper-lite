package gmaps

import (
	"net/url"
	"reflect"
	"testing"
)

func TestPlaceURLWithLangDoesNotDuplicateExistingHL(t *testing.T) {
	got := placeURLWithLang("https://www.google.com/maps/place/Test?authuser=0&hl=en&rclk=1", "en")
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}

	q := parsed.Query()
	if !reflect.DeepEqual(q["hl"], []string{"en"}) {
		t.Fatalf("hl query values = %#v, want one en", q["hl"])
	}
	if q.Get("authuser") != "0" || q.Get("rclk") != "1" {
		t.Fatalf("query values = %v, want existing parameters preserved", q)
	}
}

func TestPlaceURLWithLangReplacesExistingHL(t *testing.T) {
	got := placeURLWithLang("https://www.google.com/maps/place/Test?authuser=0&hl=fr&rclk=1", "en")
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}

	if got := parsed.Query()["hl"]; !reflect.DeepEqual(got, []string{"en"}) {
		t.Fatalf("hl query values = %#v, want one en", got)
	}
}

func TestPlaceURLWithLangAddsHL(t *testing.T) {
	tests := []string{
		"https://www.google.com/maps/place/Test",
		"https://www.google.com/maps/place/Test?authuser=0",
	}

	for _, tt := range tests {
		got := placeURLWithLang(tt, "en")
		parsed, err := url.Parse(got)
		if err != nil {
			t.Fatalf("parse URL %q: %v", got, err)
		}
		if parsed.Query().Get("hl") != "en" {
			t.Fatalf("placeURLWithLang(%q, en) = %q, want hl=en", tt, got)
		}
	}
}
