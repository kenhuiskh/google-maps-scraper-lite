# google-maps-scraper-lite

> **Based on [gosom/google-maps-scraper](https://github.com/gosom/google-maps-scraper)** by [Georgios Somarakis](https://github.com/gosom). This is a trimmed-down fork focused on a lightweight CLI experience with Playwright browser pooling and geo-spatial BIA analysis. Licensed under the [MIT License](LICENSE).

A standalone Go CLI for scraping Google Maps search results with Playwright. It supports single-query runs, geo-anchored searches, optional email extraction, optional review expansion, JSON or CSV output, Postgres output, SQLite-backed pause/resume, and a `suggest-zoom` helper for planning `-geo` coverage.

## Features

- Scrape one or more Google Maps search queries.
- Anchor searches to a map center with `-geo "lat,lng,zoomz"`.
- Filter results to a radius from the anchor with `-radius`.
- Output newline-delimited JSON or CSV.
- Write directly to Postgres with `-dsn`.
- Extract emails from business websites with `-email`.
- Scrape additional reviews with `-reviews`.
- Auto-save scraper state in SQLite and resume a blocked or paused run with `-job`.
- Use `suggest-zoom` to estimate practical zoom anchors before building a sweep plan.

## Requirements

- Go 1.22+
- A C compiler and SQLite development headers for native SQLite (`github.com/mattn/go-sqlite3`)
- Chromium installed through Playwright
- Internet access for Google Maps
- Internet access for Overpass API if you use `suggest-zoom`

## Installation

```bash
cd google-maps-scraper-lite

go run github.com/playwright-community/playwright-go/cmd/playwright install chromium
CGO_ENABLED=1 go build -o google-maps-scraper-lite .
```

### macOS Local Setup

```bash
xcode-select --install
brew install sqlite
go run github.com/playwright-community/playwright-go/cmd/playwright install chromium
CGO_ENABLED=1 go build -o google-maps-scraper-lite .
```

If Go cannot find Homebrew SQLite:

```bash
CGO_ENABLED=1 \
CGO_CFLAGS="-I$(brew --prefix sqlite)/include" \
CGO_LDFLAGS="-L$(brew --prefix sqlite)/lib" \
go build -o google-maps-scraper-lite .
```

### Ubuntu/Debian Local Setup

```bash
sudo apt-get update
sudo apt-get install -y build-essential libsqlite3-dev
go run github.com/playwright-community/playwright-go/cmd/playwright install chromium
CGO_ENABLED=1 go build -o google-maps-scraper-lite .
```

## How It Works

The scraper follows a **Feed → Place → Details** pipeline:

1. **Feed** — navigates each Google Maps search URL, scrolls the results list, and collects place URLs.
2. **Place** — visits each place URL and extracts the `APP_INITIALIZATION_STATE` JSON blob via JS injection.
3. **Details** — parses the raw JSON into a structured `Entry` (28 fields).

**Concurrency:** feeds are collected serially (one per query), then place URLs are claimed from the local SQLite state database by workers controlled by `-c`. The browser page pool matches the concurrency level — each worker acquires and releases a page around each scrape.

## CLI Usage

### Basic

```bash
./google-maps-scraper-lite -queries "coffee shops in Toronto"
./google-maps-scraper-lite -queries "pizza in Montreal,sushi in Vancouver"
./google-maps-scraper-lite -queries "dentists in Calgary" -json -o output/dentists
```

### Geo-Anchored Search

Bias the map viewport to a specific center and zoom level:

```bash
./google-maps-scraper-lite \
  -queries "restaurants,bubble tea" \
  -geo "43.6532,-79.3832,17z" \
  -depth 15 \
  -c 3 \
  -json \
  -o output/toronto-core
```

### Geo + Radius Filter

Scrape a geo-anchored area and keep only results within a given radius (meters) of the center:

```bash
./google-maps-scraper-lite \
  -queries "restaurants" \
  -geo "43.6488,-79.3773,18z" \
  -radius 350 \
  -json \
  -o output/entertainment-district
```

Note: `-radius` requires `-geo`. Results outside the radius are discarded after all place details are fetched.

### Postgres Output

```bash
./google-maps-scraper-lite \
  -queries "restaurants" \
  -geo "43.6488,-79.3773,18z" \
  -dsn "postgres://user:pass@localhost:5432/maps"
```

Table names default to `restaurants` and `restaurant_reviews` but can be overridden:

```bash
./google-maps-scraper-lite \
  -queries "restaurants" \
  -dsn "postgres://user:pass@localhost:5432/maps" \
  -table-restaurant my_places \
  -table-review my_reviews
```

The Postgres writer uses a connection pool with a 30-second per-write timeout. It deduplicates places by `cid`, `place_id`, or `data_id`, then upserts using the existing row's canonical `cid`. Reviews are inserted with `ON CONFLICT DO NOTHING`, keyed on `cid` + `reviewer_name`.

### Email Extraction and Review Expansion

```bash
./google-maps-scraper-lite \
  -queries "cafes in Toronto" \
  -email \
  -reviews 50 \
  -json \
  -o output/cafes
```

`-email` visits each business website and extracts emails from `mailto:` links and raw HTML. Social media domains (facebook.com, instagram.com, twitter.com, x.com) are skipped.

`-reviews N` fetches additional review pages until at least N reviews are collected.

### Pause and Resume

Every normal run creates a job in the local SQLite state database. The default path is `gmdata/scraper-state.sqlite`, which is inside the gitignored `gmdata/` directory. Each collected place URL is tracked as pending, in progress, done, or failed.

Google Maps may block the Playwright session mid-scrape (rate limiting or bot detection). When this happens, consecutive place scrapes begin failing. The scraper detects this after 10 consecutive failures across all workers, saves the job state, and exits with a clear message:

```
session blocked by Google — resume with: --job job_20260428_103000_123456
```

Pressing `Ctrl-C` once requests a graceful pause. Active URL scrapes finish and no new URLs are claimed. Pressing `Ctrl-C` a second time forces shutdown.

```bash
# Original run
./google-maps-scraper-lite -queries "restaurants in Toronto" -c 3 -o gmdata

# Resume by job ID
./google-maps-scraper-lite -job job_20260428_103000_123456 -c 3 -o gmdata
```

A resumed run writes its results to a **new** output file (same directory, new timestamp). If you're using `-dsn`, results from both runs land in the same database table — no manual merge needed.

You can choose a different state DB path:

```bash
./google-maps-scraper-lite \
  -queries "restaurants in Toronto" \
  -state-db gmdata/scraper-state.sqlite
```

To enable the local control UI, set the required basic-auth credentials and bind the UI to a local address:

```bash
CONTROL_USERNAME=admin CONTROL_PASSWORD=changeme \
./google-maps-scraper-lite \
  -queries "restaurants in Toronto" \
  -control-addr 127.0.0.1:8080
```

Then open `http://127.0.0.1:8080` and sign in with those credentials.

To run only the control UI against the saved SQLite jobs:

```bash
CONTROL_USERNAME=admin CONTROL_PASSWORD=changeme \
./google-maps-scraper-lite \
  -state-db gmdata/scraper-state.sqlite \
  -control-addr 127.0.0.1:8080
```

The UI lists jobs in the selected state DB. **Create Job** stores the selected parameters, saves them as a reusable template, and either starts immediately or queues behind the active job. Queued jobs start only after the previous job finishes with `done`; the saved queue wait defaults to 20 minutes. The form also lets you choose whether the concurrency value is passed as `-c` or `-max-c`. The **Pause** button requests a graceful pause. The **Resume** button starts a new local scraper process for that job using the same `-state-db`.

### Logging

```bash
./google-maps-scraper-lite \
  -queries "restaurants in Toronto" \
  -error-log logs/errors/scraper.log
```

With `-error-log`, all log output goes to both stderr and the specified file. The scraper logs:

- Query start/end with URL count and elapsed time:
  ```
  query 1/3 "restaurants" — 42 URLs found (76s)
  ```
- Place progress periodically (every Nth place or every 30s):
  ```
  places 12/42 completed, 0 failed (elapsed 112s)
  ```
- Write success/failure counts on exit.

## Flags

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `-queries` | string | required* | Comma-separated search queries. Not required when `-job` is set. |
| `-job` | string | `""` | SQLite job ID to resume. Skips the feed phase; queries and URLs are read from the state DB. |
| `-state-db` | string | `gmdata/scraper-state.sqlite` | Local SQLite state database path. |
| `-control-addr` | string | `""` | Optional local HTTP control UI address, e.g. `127.0.0.1:8080`. Can be used without `-queries`/`-job` for UI-only mode. |
| `-c` | int | `1` | Concurrency level. |
| `-max-c` | int | `0` | Maximum dynamic browser tab concurrency. Overrides `-c` when set. Tabs are opened lazily from 1 up to this ceiling. |
| `-depth` | int | `10` | Max scroll depth per query. |
| `-limit` | int | `0` | Cap the total number of places scraped. `0` = no limit. |
| `-json` | bool | `false` | Output JSON instead of CSV. |
| `-o` | string | `gmdata` | Output directory for JSON files. Ignored when `-dsn` is set. |
| `-dsn` | string | `""` | Postgres connection string. Enables DB output instead of file/stdout. |
| `-table-restaurant` | string | `restaurants` | Postgres table name for places. Used with `-dsn`. |
| `-table-review` | string | `restaurant_reviews` | Postgres table name for reviews. Used with `-dsn`. |
| `-email` | bool | `false` | Visit business websites and extract emails. |
| `-reviews` | int | `0` | Minimum number of reviews to scrape. `0` uses default page data only. |
| `-lang` | string | `en` | Browser and Maps language. |
| `-geo` | string | `""` | Search center in `lat,lng,zoomz` format, e.g. `43.6532,-79.3832,17z`. |
| `-radius` | float | `0` | Keep only results within this many meters of the `-geo` center. Requires `-geo`. |
| `-headless` | bool | `true` | Run browser headless. |
| `-error-log` | string | `""` | Append application logs to a file as well as stderr. |
| `-urls-only` | string | `""` | Debug: collect feed URLs only and write them to this file. No place scraping is performed. |

Place workers start 500ms apart and wait 500ms before each place navigation. If a place tab returns a scrape error, the job is paused, the URL is requeued, the process waits a random 10-60 minutes, and then the job resumes automatically. Press `Ctrl-C` during the countdown to cancel the wait.

## Output

### JSON

With `-json`, the tool writes one JSON object per line to a timestamped file under `-o`:

```
output/toronto-core/2026-04-12_14-30-00.json
```

Example record shape:

```json
{
  "input_id": "restaurants",
  "link": "https://www.google.com/maps/place/...",
  "cid": "1234567890",
  "title": "Example Cafe",
  "categories": ["Cafe", "Coffee shop"],
  "category": "Cafe",
  "address": "123 King St W, Toronto, ON",
  "open_hours": {"Monday": ["8:00 AM-6:00 PM"]},
  "popular_times": {"Monday": {"9": 45}},
  "web_site": "https://example.com",
  "phone": "+1 416-555-0100",
  "plus_code": "MJQW+XX Toronto, Ontario",
  "review_count": 128,
  "review_rating": 4.4,
  "reviews_per_rating": {"5": 90, "4": 20},
  "latitude": 43.645,
  "longitude": -79.39,
  "status": "Open",
  "description": "Neighbourhood coffee shop",
  "reviews_link": "https://www.google.com/maps/place/...",
  "thumbnail": "https://lh3.googleusercontent.com/...",
  "timezone": "America/Toronto",
  "price_range": "$$",
  "data_id": "0x...",
  "place_id": "ChIJ...",
  "images": [],
  "reservations": [],
  "order_online": [],
  "menu": {"link": "", "source": ""},
  "owner": {"id": "", "name": "", "link": ""},
  "complete_address": {
    "borough": "",
    "street": "123 King St W",
    "city": "Toronto",
    "postal_code": "",
    "state": "ON",
    "country": "Canada"
  },
  "about": [],
  "user_reviews": [],
  "user_reviews_extended": [],
  "emails": [],
  "review_tags": []
}
```

### CSV

Without `-json` or `-dsn`, CSV rows are written to stdout. Redirect stdout to save to a file:

```bash
./google-maps-scraper-lite -queries "coffee shops in Toronto" > results.csv
```

CSV columns:

`input_id, link, title, category, address, open_hours, popular_times, website, phone, plus_code, review_count, review_rating, reviews_per_rating, latitude, longitude, cid, status, descriptions, reviews_link, thumbnail, timezone, price_range, data_id, place_id, images, reservations, order_online, menu, owner, complete_address, about, user_reviews, user_reviews_extended, emails, review_tags`

### Postgres

When `-dsn` is set, the tool creates two tables if they do not exist and upserts on each write:

- places table (default `restaurants_<lang>`) — one row per place, deduplicated by `cid`, `place_id`, or `data_id`
- reviews table (default `restaurant_reviews_<lang>`) — one row per review, conflict key is `(cid, reviewer_name)`

Language suffixes are lower-case and non-alphanumeric separators become `_`, so `-lang zh-TW` writes to `restaurants_zh_tw` and `restaurant_reviews_zh_tw`. Table names are still configurable via `-table-restaurant` and `-table-review`. The places table keeps `cid` as the primary key and adds unique indexes for non-empty `place_id` and `data_id`; if an existing database already contains duplicates in either field, those rows must be cleaned before the indexes can be created. The connection pool handles reconnects and idle timeouts automatically. Each write uses a 30-second timeout context.

## `suggest-zoom` Subcommand

`suggest-zoom` helps choose a practical Google Maps zoom level for each `-geo` anchor before running a full scrape. It evaluates how commercially dense an area is by counting nearby food-related OSM points of interest within 500 meters, optionally adds a second signal from a Business Improvement Area (BIA) GeoJSON file, and recommends `16z`, `17z`, or `18z`.

No API key is required. Internet access is needed to query the public Overpass API.

### Scoring Model

Each point is scored from two independent signals:

| Signal | Condition | Points |
|--------|-----------|-------:|
| OSM food POI count | >= 25 within 500m | +2 |
| OSM food POI count | 8-24 within 500m | +1 |
| OSM food POI count | < 8 within 500m | +0 |
| BIA containment (optional) | Inside a BIA boundary | +2 |
| BIA containment (optional) | Outside / no file provided | +0 |

Score to zoom tier:

| Total score | Recommended zoom | Approx. anchor spacing |
|-------------|-----------------|------------------------|
| 3-4 | 18z | ~225m |
| 1-2 | 17z | ~350m |
| 0 | 16z | ~600m |

### Flags

| Flag | Description |
|------|-------------|
| `-geo "lat,lng"` | Point to score. Repeatable. Also accepts `"lat,lng,zoomz"` — the zoom part is ignored. |
| `-bia path` | Optional path to a GeoJSON `FeatureCollection` of BIA polygons. Each feature needs an `AREA_NAME` property and `Polygon` or `MultiPolygon` geometry. |

### Usage Examples

```bash
# Single point, OSM only
./google-maps-scraper-lite suggest-zoom -geo "43.6488,-79.3773"

# Multiple points
./google-maps-scraper-lite suggest-zoom \
  -geo "43.6488,-79.3773" \
  -geo "43.6690,-79.3850" \
  -geo "43.8561,-79.3370"

# With BIA GeoJSON
./google-maps-scraper-lite suggest-zoom \
  -geo "43.6488,-79.3773" \
  -geo "43.8561,-79.3370" \
  -bia toronto_bia.geojson

# Full geo strings are accepted — zoom part is ignored
./google-maps-scraper-lite suggest-zoom \
  -geo "43.6488,-79.3773,18z" \
  -geo "43.8561,-79.3370,16z"

# Batch-evaluate all anchors from gta-sweep.sh
grep -oP '"[\K[\d.]+,-[\d.]+(?=,\d+z)' gta-sweep.sh | sort -u | \
  xargs -I{} sh -c './google-maps-scraper-lite suggest-zoom -geo "{}" -bia toronto_bia.geojson'
```

### Output Format

One line per evaluated point:

```
43.6488,-79.3773  BIA: "Entertainment District BIA"       food_poi=42   score=4  → 18z
43.8561,-79.3370  no BIA                                  food_poi=6    score=0  → 16z
```

| Column | Meaning |
|--------|---------|
| `lat,lng` | The input point. |
| `BIA status` | Matched BIA name or `no BIA`. |
| `food_poi` | OSM food POI count within 500m. |
| `score` | Combined score (0-4). |
| `zoom` | Recommended zoom for `-geo`. |

### Interpreting Results

- **Score 4** (BIA + dense OSM): very high confidence → `18z`, ~225m anchor spacing.
- **Score 3** (BIA + medium OSM, or very dense OSM): high confidence → `18z`.
- **Score 2** (BIA only, or dense OSM without BIA): moderate confidence → `17z`, ~350m spacing.
- **Score 1** (medium OSM, no BIA): standard coverage → `17z`.
- **Score 0** (sparse OSM, no BIA): wide area coverage → `16z`, ~600m spacing.

### BIA File Format

- Must be a valid GeoJSON `FeatureCollection`.
- Each feature needs `AREA_NAME` in `properties` and `Polygon` or `MultiPolygon` geometry. Polygon holes are respected.
- Toronto BIA data is available from the Toronto Open Data Portal under the `Business Improvement Areas` dataset.

### Limitations

- OSM data quality varies — denser in urban cores, less complete in suburban or strip-mall areas.
- BIA boundaries change over time; refresh the GeoJSON file periodically.
- The 500m OSM search radius is a fixed heuristic, not adaptive.
- Overpass API is a free public service. Avoid firing hundreds of points rapidly; aim for roughly 1 request/second.

## Repository Layout

```
.
├── main.go            CLI entry point, flag parsing, output writer setup
├── suggest_zoom.go    suggest-zoom subcommand
├── browser/           Playwright lifecycle and thread-safe page pool
├── geo/               Overpass client, BIA index, zoom scoring logic
├── gmaps/
│   ├── jobstore.go    SQLite job state, pause/resume progress, block detection
│   ├── feed.go        Navigate search, scroll results, return place URLs
│   ├── place.go       Navigate place URL, extract APP_INITIALIZATION_STATE JSON
│   ├── entry.go       Entry struct (28 fields) and all JSON parsing logic
│   ├── email.go       Fetch business website HTML, extract emails
│   ├── multiple.go    Parse bulk JSON search result format
│   └── scraper.go     Orchestrator: serial feeds, concurrent place fan-out
└── output/            Writer interface with CSV, JSON, and Postgres implementations
```

## Notes

- Place data comes from Google Maps' `window.APP_INITIALIZATION_STATE` JavaScript variable. The JSON structure has at least two known variants; parsing handles both with fallback logic.
- When `-radius` is set, all place details are fetched first and the filter is applied after `g.Wait()` completes. No records are written until the full fan-out finishes.
- `suggest-zoom` depends on the public Overpass API. Use reasonable request pacing for bulk point scoring.
- Deduplicate multi-zone output by `cid` field, or by `place_id`/`data_id` when you need stricter matching:
  ```bash
  jq -s '[.[]] | unique_by(.cid)' output/**/*.json > deduped.json
  ```

## Credits

This project is a fork of [gosom/google-maps-scraper](https://github.com/gosom/google-maps-scraper) by [Georgios Somarakis](https://github.com/gosom). The original project provided the core scraping pipeline and data extraction approach that this fork builds upon.

## License

[MIT](LICENSE)
