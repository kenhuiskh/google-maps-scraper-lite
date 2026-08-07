package gmaps

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// placeURLCoordsRE matches the latitude/longitude Maps embeds in a place URL's
// data= parameter: `!8m2!3d<lat>!4d<lon>`. Every place URL the feed produces
// carries it, which is what lets the radius filter run before the scrape rather
// than after it.
var placeURLCoordsRE = regexp.MustCompile(`!3d(-?\d+(?:\.\d+)?)!4d(-?\d+(?:\.\d+)?)`)

// ParseGeoCenter extracts the latitude and longitude from a geo string of the
// form "lat,lng" or "lat,lng,zoomz" (e.g. "43.6532,-79.3832,14z").
func ParseGeoCenter(geo string) (lat, lon float64, err error) {
	parts := strings.SplitN(geo, ",", 3)
	if len(parts) < 2 {
		return 0, 0, fmt.Errorf("geo %q: expected at least lat,lng", geo)
	}

	lat, err = strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	if err != nil {
		return 0, 0, fmt.Errorf("geo %q: invalid latitude: %w", geo, err)
	}

	lon, err = strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if err != nil {
		return 0, 0, fmt.Errorf("geo %q: invalid longitude: %w", geo, err)
	}

	return lat, lon, nil
}

// CoordsFromPlaceURL extracts a place's coordinates from its Maps URL without
// visiting it. ok is false when the URL carries no parseable pair, so callers can
// tell "outside the radius" from "unmeasurable" and keep the latter.
func CoordsFromPlaceURL(placeURL string) (lat, lon float64, ok bool) {
	match := placeURLCoordsRE.FindStringSubmatch(placeURL)
	if len(match) != 3 {
		return 0, 0, false
	}

	lat, err := strconv.ParseFloat(match[1], 64)
	if err != nil {
		return 0, 0, false
	}

	lon, err = strconv.ParseFloat(match[2], 64)
	if err != nil {
		return 0, 0, false
	}

	return lat, lon, true
}

// haversineMeters returns the great-circle distance in meters between two
// coordinate pairs.
func haversineMeters(lat1, lon1, lat2, lon2 float64) float64 {
	const R = 6371e3 // earth radius in meters

	rlat1 := lat1 * math.Pi / 180
	rlon1 := lon1 * math.Pi / 180

	rlat2 := lat2 * math.Pi / 180
	rlon2 := lon2 * math.Pi / 180

	dlat := rlat2 - rlat1
	dlon := rlon2 - rlon1

	a := math.Sin(dlat/2)*math.Sin(dlat/2) +
		math.Cos(rlat1)*math.Cos(rlat2)*
			math.Sin(dlon/2)*math.Sin(dlon/2)

	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))

	return R * c
}
