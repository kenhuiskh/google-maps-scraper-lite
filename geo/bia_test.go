package geo_test

import (
	"os"
	"strings"
	"testing"

	"github.com/gosom/google-maps-scraper-lite/geo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func loadSampleBIA(t *testing.T) *geo.BIAIndex {
	t.Helper()
	f, err := os.Open("testdata/toronto_bia_sample.geojson")
	require.NoError(t, err)
	defer f.Close()
	idx, err := geo.LoadBIAIndex(f)
	require.NoError(t, err)
	return idx
}

func TestBIAIndex_SimplePolygon_Inside(t *testing.T) {
	idx := loadSampleBIA(t)
	name, inside := idx.Lookup(43.65, -79.38)
	assert.True(t, inside)
	assert.Equal(t, "Simple Polygon BIA", name)
}

func TestBIAIndex_SimplePolygon_Outside(t *testing.T) {
	idx := loadSampleBIA(t)
	_, inside := idx.Lookup(43.60, -79.38)
	assert.False(t, inside)
}

func TestBIAIndex_MultiPolygon_Inside(t *testing.T) {
	idx := loadSampleBIA(t)
	name, inside := idx.Lookup(43.71, -79.49)
	assert.True(t, inside)
	assert.Equal(t, "MultiPolygon BIA", name)
}

func TestBIAIndex_Donut_Ring_Inside_NotHole(t *testing.T) {
	idx := loadSampleBIA(t)
	name, inside := idx.Lookup(43.689, -79.419)
	assert.True(t, inside)
	assert.Equal(t, "Donut BIA", name)
}

func TestBIAIndex_Donut_InsideHole_NotInBIA(t *testing.T) {
	idx := loadSampleBIA(t)
	_, inside := idx.Lookup(43.690, -79.410)
	assert.False(t, inside)
}

func TestNewBIAIndexFromFeatures_Empty(t *testing.T) {
	idx := geo.NewBIAIndexFromFeatures(nil)
	_, inside := idx.Lookup(43.65, -79.38)
	assert.False(t, inside)
}

func TestLoadBIAIndex_InvalidJSON(t *testing.T) {
	_, err := geo.LoadBIAIndex(strings.NewReader("not json"))
	require.Error(t, err)
}
