package gmaps

import (
	"math"
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

func Test_CoordsFromPlaceURL(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantLat float64
		wantLon float64
		wantOK  bool
	}{
		{
			// Verbatim from a job log: the shape every feed-produced URL has.
			name:    "place url from feed",
			url:     "https://www.google.com/maps/place/Fahrenheit+Coffee/data=!4m7!3m6!1s0x882b34ddcf3bd6ef:0x23da248aeab1f5de!8m2!3d43.6469915!4d-79.4006438!16s%2Fg%2F11csr05c32!19sChIJ79Y7z900K4gR3vWx6ook2iM?authuser=0&hl=en&rclk=1",
			wantLat: 43.6469915,
			wantLon: -79.4006438,
			wantOK:  true,
		},
		{
			name:    "percent-encoded place name",
			url:     "https://www.google.com/maps/place/%E5%A4%A9%E6%B4%A5%E5%8C%85%E5%AD%90%E9%93%BA/data=!4m7!3m6!1s0x882b2b75597bcc8f:0x3c4bfa3ddc5fe4a1!8m2!3d43.8443658!4d-79.3886781!16s%2Fg%2F11z7nm0_zk",
			wantLat: 43.8443658,
			wantLon: -79.3886781,
			wantOK:  true,
		},
		{
			name:    "southern and western hemispheres",
			url:     "https://www.google.com/maps/place/Somewhere/data=!8m2!3d-33.8688!4d-151.2093",
			wantLat: -33.8688,
			wantLon: -151.2093,
			wantOK:  true,
		},
		{
			name:    "integer coordinates",
			url:     "https://www.google.com/maps/place/Null+Island/data=!8m2!3d0!4d0",
			wantLat: 0,
			wantLon: 0,
			wantOK:  true,
		},
		{
			// A place-ID URL carries no coordinates; the caller must keep it.
			name:   "no coordinates",
			url:    "https://www.google.com/maps/place/?q=place_id:ChIJ79Y7z900K4gR3vWx6ook2iM",
			wantOK: false,
		},
		{
			name:   "lat without lon",
			url:    "https://www.google.com/maps/place/Half/data=!8m2!3d43.6469915",
			wantOK: false,
		},
		{
			name:   "empty",
			url:    "",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lat, lon, ok := CoordsFromPlaceURL(tt.url)
			if ok != tt.wantOK {
				t.Fatalf("ok: got %v, want %v", ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
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

func Test_haversineMeters(t *testing.T) {
	tests := []struct {
		name                   string
		lat1, lon1, lat2, lon2 float64
		want                   float64
	}{
		{name: "identical points", lat1: 43.65, lon1: -79.38, lat2: 43.65, lon2: -79.38, want: 0},
		{name: "one degree of latitude", lat1: 0, lon1: 0, lat2: 1, lon2: 0, want: 111194.93},
		{
			name: "downtown toronto to a nearby cafe",
			lat1: 43.6469915, lon1: -79.4006438, lat2: 43.6532, lon2: -79.3832,
			want: 1564.08,
		},
		{
			name: "downtown toronto to a suburban place",
			lat1: 43.8142606, lon1: -79.4247181, lat2: 43.6532, lon2: -79.3832,
			want: 18217.13,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := haversineMeters(tt.lat1, tt.lon1, tt.lat2, tt.lon2)
			if math.Abs(got-tt.want) > 0.01 {
				t.Errorf("haversineMeters() = %v, want %v", got, tt.want)
			}
		})
	}
}

// The Entry method must stay a pure delegation: the post-scrape radius filter and
// the pre-scrape URL filter have to agree on distance, or the cheap filter could
// drop a place the authoritative one would have kept.
func Test_haversineDistance_matchesHaversineMeters(t *testing.T) {
	entry := &Entry{Latitude: 43.6469915, Longitude: -79.4006438}

	got := entry.haversineDistance(43.6532, -79.3832)
	want := haversineMeters(entry.Latitude, entry.Longitude, 43.6532, -79.3832)

	if got != want {
		t.Errorf("haversineDistance() = %v, haversineMeters() = %v", got, want)
	}
}
