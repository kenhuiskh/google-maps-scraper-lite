package main

import "testing"

func TestParseLangs(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", []string{"en"}},
		{"en", []string{"en"}},
		{"en,fr", []string{"en", "fr"}},
		{" EN , Fr ", []string{"en", "fr"}},
		{"en,en", []string{"en"}},
	}
	for _, tc := range cases {
		got, err := parseLangs(tc.in)
		if err != nil {
			t.Fatalf("parseLangs(%q) error: %v", tc.in, err)
		}
		if len(got) != len(tc.want) {
			t.Fatalf("parseLangs(%q) = %#v, want %#v", tc.in, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("parseLangs(%q) = %#v, want %#v", tc.in, got, tc.want)
			}
		}
	}
}

func TestParseLangsRejectsSuffixCollision(t *testing.T) {
	if _, err := parseLangs("en-US,en_US"); err == nil {
		t.Fatal("expected collision error for en-US and en_US")
	}
}
