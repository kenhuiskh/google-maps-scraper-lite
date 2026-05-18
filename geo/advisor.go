package geo

import (
	"context"
	"time"
)

// Point is a geographic coordinate.
type Point struct {
	Lat float64
	Lng float64
}

// Suggestion is the zoom recommendation for one Point.
type Suggestion struct {
	Point    Point
	BIAName  string // empty when not inside a BIA or no BIA file provided
	FoodPOIs int
	Score    int    // 0-4: OSM score (0/1/2) + BIA score (0/2)
	Zoom     string // "18z", "17z", or "16z"
}

// FoodPOICounter fetches the number of food POIs near a point.
type FoodPOICounter interface {
	FoodPOICount(ctx context.Context, lat, lng float64, radiusMeters int) (int, error)
}

// SuggestZoom returns a zoom recommendation for each point.
// idx may be nil when no BIA file is provided; BIA score is 0 in that case.
func SuggestZoom(ctx context.Context, idx *BIAIndex, counter FoodPOICounter, points []Point) ([]Suggestion, error) {
	results := make([]Suggestion, 0, len(points))
	for i, p := range points {
		if i > 0 {
			timer := time.NewTimer(1100 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
		s := Suggestion{Point: p}

		// BIA signal (+0 or +2)
		if idx != nil {
			if name, inside := idx.Lookup(p.Lat, p.Lng); inside {
				s.BIAName = name
				s.Score += 2
			}
		}

		// OSM food POI signal (+0, +1, or +2)
		n, err := counter.FoodPOICount(ctx, p.Lat, p.Lng, 500)
		if err != nil {
			return nil, err
		}
		s.FoodPOIs = n
		switch {
		case n >= 25:
			s.Score += 2
		case n >= 8:
			s.Score += 1
		}

		// Score -> zoom
		switch {
		case s.Score >= 3:
			s.Zoom = "18z"
		case s.Score >= 1:
			s.Zoom = "17z"
		default:
			s.Zoom = "16z"
		}

		results = append(results, s)
	}
	return results, nil
}
