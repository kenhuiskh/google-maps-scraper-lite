package geo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const foodAmenityPattern = `^(restaurant|fast_food|cafe|bar|pub|food_court|ice_cream)$`

// BuildOverpassFoodCountQuery returns the Overpass QL query for food POIs near a point.
func BuildOverpassFoodCountQuery(lat, lng float64, radiusMeters int) string {
	coord := fmt.Sprintf("%f,%f", lat, lng)
	around := fmt.Sprintf("around:%d,%s", radiusMeters, coord)
	tag := fmt.Sprintf(`["amenity"~"%s"]`, foodAmenityPattern)
	return fmt.Sprintf(`[out:json][timeout:25];
(
  node%s(%s);
  way%s(%s);
  relation%s(%s);
);
out count;`, tag, around, tag, around, tag, around)
}

type overpassCountResponse struct {
	Elements []struct {
		Tags struct {
			Total string `json:"total"`
		} `json:"tags"`
	} `json:"elements"`
}

// OverpassClient calls the Overpass API interpreter.
type OverpassClient struct {
	endpoint string
	http     *http.Client
}

// NewOverpassClient returns a client pointed at endpoint (production or httptest URL).
func NewOverpassClient(endpoint string) *OverpassClient {
	return &OverpassClient{endpoint: endpoint, http: &http.Client{Timeout: 30 * time.Second}}
}

// FoodPOICount returns the number of food POIs within radiusMeters of (lat, lng).
// It implements the FoodPOICounter interface.
// Transient server errors (429, 502, 503, 504) are retried up to 3 times with exponential backoff.
func (c *OverpassClient) FoodPOICount(ctx context.Context, lat, lng float64, radiusMeters int) (int, error) {
	query := BuildOverpassFoodCountQuery(lat, lng, radiusMeters)
	form := url.Values{"data": {query}}

	delays := []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, strings.NewReader(form.Encode()))
		if err != nil {
			return 0, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		resp, err := c.http.Do(req)
		if err != nil {
			return 0, err
		}

		if resp.StatusCode == http.StatusOK {
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				return 0, err
			}
			var parsed overpassCountResponse
			if err := json.Unmarshal(body, &parsed); err != nil {
				return 0, fmt.Errorf("overpass parse error: %w", err)
			}
			if len(parsed.Elements) == 0 {
				return 0, nil
			}
			n, err := strconv.Atoi(parsed.Elements[0].Tags.Total)
			if err != nil {
				return 0, fmt.Errorf("overpass total not an int: %w", err)
			}
			return n, nil
		}

		status := resp.StatusCode
		resp.Body.Close()

		retryable := status == http.StatusTooManyRequests ||
			status == http.StatusBadGateway ||
			status == http.StatusServiceUnavailable ||
			status == http.StatusGatewayTimeout
		if !retryable || attempt >= len(delays) {
			return 0, fmt.Errorf("overpass returned HTTP %d", status)
		}

		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(delays[attempt]):
		}
	}
}
