package gmaps

import (
	"testing"
)

func Test_ParseGeoCenter(t *testing.T) {
	tests := []struct {
		name    string
		geo     string
		wantLat float64
		wantLon float64
		wantErr bool
	}{
		{
			name:    "valid with zoom",
			geo:     "43.6532,-79.3832,14z",
			wantLat: 43.6532,
			wantLon: -79.3832,
		},
		{
			name:    "valid without zoom",
			geo:     "43.6532,-79.3832",
			wantLat: 43.6532,
			wantLon: -79.3832,
		},
		{
			name:    "empty string",
			geo:     "",
			wantErr: true,
		},
		{
			name:    "only one part",
			geo:     "43.6532",
			wantErr: true,
		},
		{
			name:    "invalid lat",
			geo:     "abc,-79.3832,14z",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lat, lon, err := ParseGeoCenter(tt.geo)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if lat != tt.wantLat {
				t.Errorf("lat: got %v, want %v", lat, tt.wantLat)
			}
			if lon != tt.wantLon {
				t.Errorf("lon: got %v, want %v", lon, tt.wantLon)
			}
		})
	}
}
