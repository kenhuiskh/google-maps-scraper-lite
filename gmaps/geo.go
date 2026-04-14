package gmaps

import (
	"fmt"
	"strconv"
	"strings"
)

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
