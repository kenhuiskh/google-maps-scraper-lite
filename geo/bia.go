package geo

import (
	"io"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/geojson"
	"github.com/paulmach/orb/planar"
)

// BIAFeature is used to build a BIAIndex from code (tests/stubs).
type BIAFeature struct {
	Name string
	Ring [][2]float64 // GeoJSON order: [lng, lat], ring must be closed
}

type biaEntry struct {
	name     string
	geometry orb.Geometry
}

// BIAIndex holds parsed BIA features for point-in-polygon lookup.
type BIAIndex struct {
	entries []biaEntry
}

// LoadBIAIndex reads a GeoJSON FeatureCollection from r and returns a BIAIndex.
// Each feature must have an "AREA_NAME" property.
func LoadBIAIndex(r io.Reader) (*BIAIndex, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	fc, err := geojson.UnmarshalFeatureCollection(raw)
	if err != nil {
		return nil, err
	}
	idx := &BIAIndex{}
	for _, f := range fc.Features {
		name, _ := f.Properties["AREA_NAME"].(string)
		idx.entries = append(idx.entries, biaEntry{name: name, geometry: f.Geometry})
	}
	return idx, nil
}

// NewBIAIndexFromFeatures builds a BIAIndex from BIAFeature values (test helper).
func NewBIAIndexFromFeatures(features []BIAFeature) *BIAIndex {
	idx := &BIAIndex{}
	for _, f := range features {
		ring := make(orb.Ring, len(f.Ring))
		for i, pt := range f.Ring {
			ring[i] = orb.Point{pt[0], pt[1]} // [lng, lat]
		}
		idx.entries = append(idx.entries, biaEntry{
			name:     f.Name,
			geometry: orb.Polygon{ring},
		})
	}
	return idx
}

// Lookup returns the BIA name and true if (lat, lng) falls inside any BIA feature.
func (idx *BIAIndex) Lookup(lat, lng float64) (string, bool) {
	pt := orb.Point{lng, lat} // GeoJSON: [lng, lat]
	for _, e := range idx.entries {
		if containsPoint(e.geometry, pt) {
			return e.name, true
		}
	}
	return "", false
}

func containsPoint(g orb.Geometry, pt orb.Point) bool {
	switch geom := g.(type) {
	case orb.Polygon:
		return planar.PolygonContains(geom, pt)
	case orb.MultiPolygon:
		return planar.MultiPolygonContains(geom, pt)
	}
	return false
}
