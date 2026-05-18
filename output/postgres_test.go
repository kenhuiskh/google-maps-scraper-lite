package output

import (
	"strings"
	"testing"
)

func TestValidatePostgresIdentifier(t *testing.T) {
	valid := []string{"restaurants", "_x", "Foo_Bar9", "a", strings.Repeat("a", 63)}
	for _, s := range valid {
		if err := validatePostgresIdentifier(s); err != nil {
			t.Fatalf("expected %q valid, got %v", s, err)
		}
	}
	invalid := []string{"", "1abc", "foo;DROP TABLE x", "foo bar", "foo\"bar", "foo-bar", "foo.bar", strings.Repeat("a", 64)}
	for _, s := range invalid {
		if err := validatePostgresIdentifier(s); err == nil {
			t.Fatalf("expected %q invalid, got nil", s)
		}
	}
}

func TestPostgresIndexName(t *testing.T) {
	tests := []struct {
		table string
		want  string
	}{
		{table: "restaurants", want: "restaurants"},
		{table: "public.restaurants", want: "public_restaurants"},
		{table: "my-restaurants", want: "my_restaurants"},
		{table: "", want: "restaurants"},
	}

	for _, tt := range tests {
		t.Run(tt.table, func(t *testing.T) {
			if got := postgresIndexName(tt.table); got != tt.want {
				t.Fatalf("postgresIndexName(%q) = %q, want %q", tt.table, got, tt.want)
			}
		})
	}
}
