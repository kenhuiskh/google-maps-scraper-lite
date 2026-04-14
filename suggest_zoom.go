package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gosom/google-maps-scraper-lite/geo"
)

// runSuggestZoom is the entry point for the suggest-zoom subcommand.
func runSuggestZoom(args []string) {
	fs := flag.NewFlagSet("suggest-zoom", flag.ExitOnError)
	var geoFlags multiStringFlag
	fs.Var(&geoFlags, "geo", `lat,lng point to evaluate (repeatable, accepts "lat,lng" or "lat,lng,zoomz")`)
	biaFile := fs.String("bia", "", "optional path to a BIA GeoJSON file for extra scoring signal")
	_ = fs.Parse(args)

	if len(geoFlags) == 0 {
		fmt.Fprintln(os.Stderr, "error: at least one -geo flag is required")
		fs.Usage()
		os.Exit(1)
	}

	points := make([]geo.Point, 0, len(geoFlags))
	for _, g := range geoFlags {
		p, err := parseLatLngArg(g)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: invalid -geo %q: %v\n", g, err)
			os.Exit(1)
		}
		points = append(points, p)
	}

	var idx *geo.BIAIndex
	if *biaFile != "" {
		f, err := os.Open(*biaFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error opening -bia file: %v\n", err)
			os.Exit(1)
		}
		defer f.Close()
		idx, err = geo.LoadBIAIndex(f)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error loading -bia file: %v\n", err)
			os.Exit(1)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	client := geo.NewOverpassClient("https://overpass-api.de/api/interpreter")
	results, err := geo.SuggestZoom(ctx, idx, client, points)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	for _, r := range results {
		biaInfo := "no BIA"
		if r.BIAName != "" {
			biaInfo = fmt.Sprintf("BIA: %q", r.BIAName)
		}
		fmt.Printf("%.4f,%.4f  %-45s  food_poi=%-3d  score=%d  → %s\n",
			r.Point.Lat, r.Point.Lng, biaInfo, r.FoodPOIs, r.Score, r.Zoom)
	}
}

// multiStringFlag accumulates repeated flag values.
type multiStringFlag []string

func (m *multiStringFlag) String() string { return strings.Join(*m, ", ") }
func (m *multiStringFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

// parseLatLngArg accepts "lat,lng" or "lat,lng,zoomz" (zoom part is ignored).
func parseLatLngArg(s string) (geo.Point, error) {
	parts := strings.SplitN(s, ",", 3)
	if len(parts) < 2 {
		return geo.Point{}, fmt.Errorf("expected lat,lng got %q", s)
	}
	lat, err := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	if err != nil {
		return geo.Point{}, fmt.Errorf("bad lat: %w", err)
	}
	lng, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if err != nil {
		return geo.Point{}, fmt.Errorf("bad lng: %w", err)
	}
	return geo.Point{Lat: lat, Lng: lng}, nil
}
