package geo_test

import (
	"context"
	"testing"

	"github.com/gosom/google-maps-scraper-lite/geo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubCounter struct{ n int }

func (s stubCounter) FoodPOICount(_ context.Context, _, _ float64, _ int) (int, error) {
	return s.n, nil
}

func insideBIAIndex(t *testing.T) *geo.BIAIndex {
	t.Helper()
	return geo.NewBIAIndexFromFeatures([]geo.BIAFeature{
		{Name: "Test BIA", Ring: [][2]float64{
			{-79.39, 43.64}, {-79.37, 43.64}, {-79.37, 43.66},
			{-79.39, 43.66}, {-79.39, 43.64},
		}},
	})
}

func emptyBIAIndex() *geo.BIAIndex {
	return geo.NewBIAIndexFromFeatures(nil)
}

func TestSuggestZoom_BIA_DensePOI_Score4_18z(t *testing.T) {
	pts := []geo.Point{{Lat: 43.65, Lng: -79.38}}
	results, err := geo.SuggestZoom(context.Background(), insideBIAIndex(t), stubCounter{n: 30}, pts)
	require.NoError(t, err)
	assert.Equal(t, "18z", results[0].Zoom)
	assert.Equal(t, 4, results[0].Score)
}

func TestSuggestZoom_BIA_MidPOI_Score3_18z(t *testing.T) {
	pts := []geo.Point{{Lat: 43.65, Lng: -79.38}}
	results, err := geo.SuggestZoom(context.Background(), insideBIAIndex(t), stubCounter{n: 12}, pts)
	require.NoError(t, err)
	assert.Equal(t, "18z", results[0].Zoom)
	assert.Equal(t, 3, results[0].Score)
}

func TestSuggestZoom_BIA_SparsePOI_Score2_17z(t *testing.T) {
	pts := []geo.Point{{Lat: 43.65, Lng: -79.38}}
	results, err := geo.SuggestZoom(context.Background(), insideBIAIndex(t), stubCounter{n: 3}, pts)
	require.NoError(t, err)
	assert.Equal(t, "17z", results[0].Zoom)
	assert.Equal(t, 2, results[0].Score)
	assert.Equal(t, "Test BIA", results[0].BIAName)
}

func TestSuggestZoom_NoBIA_DensePOI_Score2_17z(t *testing.T) {
	pts := []geo.Point{{Lat: 43.85, Lng: -79.33}}
	results, err := geo.SuggestZoom(context.Background(), emptyBIAIndex(), stubCounter{n: 25}, pts)
	require.NoError(t, err)
	assert.Equal(t, "17z", results[0].Zoom)
	assert.Equal(t, 2, results[0].Score)
}

func TestSuggestZoom_NoBIA_MidPOI_Score1_17z(t *testing.T) {
	pts := []geo.Point{{Lat: 43.85, Lng: -79.33}}
	results, err := geo.SuggestZoom(context.Background(), emptyBIAIndex(), stubCounter{n: 12}, pts)
	require.NoError(t, err)
	assert.Equal(t, "17z", results[0].Zoom)
	assert.Equal(t, 1, results[0].Score)
}

func TestSuggestZoom_NoBIA_SparsePOI_Score0_16z(t *testing.T) {
	pts := []geo.Point{{Lat: 43.85, Lng: -79.33}}
	results, err := geo.SuggestZoom(context.Background(), emptyBIAIndex(), stubCounter{n: 5}, pts)
	require.NoError(t, err)
	assert.Equal(t, "16z", results[0].Zoom)
	assert.Equal(t, 0, results[0].Score)
}

func TestSuggestZoom_NilBIAIndex_NoBIAScore(t *testing.T) {
	pts := []geo.Point{{Lat: 43.85, Lng: -79.33}}
	results, err := geo.SuggestZoom(context.Background(), nil, stubCounter{n: 5}, pts)
	require.NoError(t, err)
	assert.Equal(t, "16z", results[0].Zoom)
	assert.Equal(t, 0, results[0].Score)
	assert.Equal(t, "", results[0].BIAName)
}

func TestSuggestZoom_MultiplePoints(t *testing.T) {
	pts := []geo.Point{
		{Lat: 43.65, Lng: -79.38}, // inside BIA, sparse POI -> score=2 -> 17z
		{Lat: 43.85, Lng: -79.33}, // outside BIA, sparse POI -> score=0 -> 16z
	}
	results, err := geo.SuggestZoom(context.Background(), insideBIAIndex(t), stubCounter{n: 5}, pts)
	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, "17z", results[0].Zoom)
	assert.Equal(t, "16z", results[1].Zoom)
}
