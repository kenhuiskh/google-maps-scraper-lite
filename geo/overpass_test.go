package geo_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gosom/google-maps-scraper-lite/geo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildOverpassFoodCountQuery_ContainsExpectedParts(t *testing.T) {
	q := geo.BuildOverpassFoodCountQuery(43.6488, -79.3773, 500)
	assert.Contains(t, q, "around:500,43.648800,-79.377300")
	assert.Contains(t, q, "restaurant")
	assert.Contains(t, q, "fast_food")
	assert.Contains(t, q, "out count;")
}

func TestBuildOverpassFoodCountQuery_NoScientificNotation(t *testing.T) {
	q := geo.BuildOverpassFoodCountQuery(43.123456789, -79.987654321, 500)
	assert.NotContains(t, q, "e+")
	assert.NotContains(t, q, "e-")
}

func fixtureServer(t *testing.T, filename string) *httptest.Server {
	t.Helper()
	data, err := os.ReadFile("testdata/" + filename)
	require.NoError(t, err)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	}))
}

func TestOverpassClient_Count42(t *testing.T) {
	srv := fixtureServer(t, "overpass_count_42.json")
	defer srv.Close()
	c := geo.NewOverpassClient(srv.URL)
	n, err := c.FoodPOICount(context.Background(), 43.6488, -79.3773, 500)
	require.NoError(t, err)
	assert.Equal(t, 42, n)
}

func TestOverpassClient_Count12(t *testing.T) {
	srv := fixtureServer(t, "overpass_count_12.json")
	defer srv.Close()
	c := geo.NewOverpassClient(srv.URL)
	n, err := c.FoodPOICount(context.Background(), 43.6488, -79.3773, 500)
	require.NoError(t, err)
	assert.Equal(t, 12, n)
}

func TestOverpassClient_Count6(t *testing.T) {
	srv := fixtureServer(t, "overpass_count_6.json")
	defer srv.Close()
	c := geo.NewOverpassClient(srv.URL)
	n, err := c.FoodPOICount(context.Background(), 43.6488, -79.3773, 500)
	require.NoError(t, err)
	assert.Equal(t, 6, n)
}

func TestOverpassClient_SendsRequiredHeaders(t *testing.T) {
	data, err := os.ReadFile("testdata/overpass_count_42.json")
	require.NoError(t, err)

	var userAgent, contentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		userAgent = req.Header.Get("User-Agent")
		contentType = req.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}))
	defer srv.Close()

	c := geo.NewOverpassClient(srv.URL)
	n, err := c.FoodPOICount(context.Background(), 43.6488, -79.3773, 500)
	require.NoError(t, err)
	assert.Equal(t, 42, n)
	assert.Equal(t, "google-maps-scraper-lite (+https://github.com/gosom/google-maps-scraper-lite)", userAgent)
	assert.Equal(t, "application/x-www-form-urlencoded", contentType)
}

func TestOverpassClient_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := geo.NewOverpassClient(srv.URL)
	_, err := c.FoodPOICount(context.Background(), 43.6488, -79.3773, 500)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "429")
}

func TestOverpassClient_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`not json`))
	}))
	defer srv.Close()
	c := geo.NewOverpassClient(srv.URL)
	_, err := c.FoodPOICount(context.Background(), 43.6488, -79.3773, 500)
	require.Error(t, err)
}

func TestOverpassClient_ContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		<-req.Context().Done()
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := geo.NewOverpassClient(srv.URL)
	_, err := c.FoodPOICount(ctx, 43.6488, -79.3773, 500)
	require.Error(t, err)
}
