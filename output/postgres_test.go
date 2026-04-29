package output

import "testing"

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
