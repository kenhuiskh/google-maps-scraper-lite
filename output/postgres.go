package output

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gosom/google-maps-scraper-lite/gmaps"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const createRestaurantsSQLFmt = `
CREATE TABLE IF NOT EXISTS %s (
	cid                TEXT        PRIMARY KEY,
	input_id           TEXT,
	link               TEXT,
	title              TEXT,
	categories         JSONB,
	category           TEXT,
	address            TEXT,
	open_hours         JSONB,
	popular_times      JSONB,
	web_site           TEXT,
	phone              TEXT,
	plus_code          TEXT,
	review_count       INTEGER,
	review_rating      NUMERIC,
	reviews_per_rating JSONB,
	latitude           NUMERIC,
	longitude          NUMERIC,
	status             TEXT,
	description        TEXT,
	reviews_link       TEXT,
	thumbnail          TEXT,
	timezone           TEXT,
	price_range        TEXT,
	data_id            TEXT,
	place_id           TEXT,
	images             JSONB,
	reservations       JSONB,
	order_online       JSONB,
	menu               JSONB,
	owner              JSONB,
	complete_address   JSONB,
	about              JSONB,
	emails             TEXT[],
	review_tags        JSONB,
	created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	closed_at          TIMESTAMPTZ
)`

const createReviewsSQLFmt = `
CREATE TABLE IF NOT EXISTS %s (
	id              BIGSERIAL   PRIMARY KEY,
	cid             TEXT        NOT NULL REFERENCES %s(cid) ON DELETE CASCADE,
	reviewer_name   TEXT,
	profile_picture TEXT,
	rating          INTEGER,
	description     TEXT,
	images          JSONB,
	reviewed_at     DATE,
	created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	UNIQUE (cid, reviewer_name)
)`

const createRestaurantPlaceIDIndexSQLFmt = `
CREATE UNIQUE INDEX IF NOT EXISTS %s
ON %s (place_id)
WHERE place_id IS NOT NULL AND place_id <> ''`

const createRestaurantDataIDIndexSQLFmt = `
CREATE UNIQUE INDEX IF NOT EXISTS %s
ON %s (data_id)
WHERE data_id IS NOT NULL AND data_id <> ''`

type PostgresWriter struct {
	pool            *pgxpool.Pool
	written         int64
	failed          int64
	tableRestaurant string
	tableReview     string
}

// validatePostgresIdentifier enforces a strict allow-list for table identifiers
// before they are interpolated into DDL/DML, blocking SQL injection via CLI flags.
var validPostgresIdentifierRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validatePostgresIdentifier(name string) error {
	if name == "" {
		return errors.New("identifier is empty")
	}
	if len(name) > 63 {
		return fmt.Errorf("identifier %q exceeds 63 characters", name)
	}
	if !validPostgresIdentifierRe.MatchString(name) {
		return fmt.Errorf("invalid postgres identifier %q (allowed: ^[A-Za-z_][A-Za-z0-9_]*$)", name)
	}
	return nil
}

func quotePostgresIdent(name string) string {
	return pgx.Identifier{name}.Sanitize()
}

func NewPostgresWriter(ctx context.Context, dsn, tableRestaurant, tableReview string) (*PostgresWriter, error) {
	if err := validatePostgresIdentifier(tableRestaurant); err != nil {
		return nil, fmt.Errorf("invalid restaurant table name: %w", err)
	}
	if err := validatePostgresIdentifier(tableReview); err != nil {
		return nil, fmt.Errorf("invalid review table name: %w", err)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect to postgres: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	qRestaurant := quotePostgresIdent(tableRestaurant)
	qReview := quotePostgresIdent(tableReview)

	if _, err := pool.Exec(ctx, fmt.Sprintf(createRestaurantsSQLFmt, qRestaurant)); err != nil {
		pool.Close()
		return nil, fmt.Errorf("create %s table: %w", tableRestaurant, err)
	}

	indexPrefix := postgresIndexName(tableRestaurant)
	qPlaceIDIndex := quotePostgresIdent(indexPrefix + "_place_id_unique")
	qDataIDIndex := quotePostgresIdent(indexPrefix + "_data_id_unique")
	if _, err := pool.Exec(ctx, fmt.Sprintf(createRestaurantPlaceIDIndexSQLFmt, qPlaceIDIndex, qRestaurant)); err != nil {
		pool.Close()
		return nil, fmt.Errorf("create %s place_id unique index: %w", tableRestaurant, err)
	}

	if _, err := pool.Exec(ctx, fmt.Sprintf(createRestaurantDataIDIndexSQLFmt, qDataIDIndex, qRestaurant)); err != nil {
		pool.Close()
		return nil, fmt.Errorf("create %s data_id unique index: %w", tableRestaurant, err)
	}

	if _, err := pool.Exec(ctx, fmt.Sprintf(createReviewsSQLFmt, qReview, qRestaurant)); err != nil {
		pool.Close()
		return nil, fmt.Errorf("create %s table: %w", tableReview, err)
	}

	return &PostgresWriter{pool: pool, tableRestaurant: tableRestaurant, tableReview: tableReview}, nil
}

func postgresIndexName(table string) string {
	re := regexp.MustCompile(`[^a-zA-Z0-9_]`)
	name := re.ReplaceAllString(table, "_")
	name = strings.Trim(name, "_")
	if name == "" {
		return "restaurants"
	}
	return name
}

func (p *PostgresWriter) Write(entry *gmaps.Entry) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	toJSON := func(field string, v any) []byte {
		b, err := json.Marshal(v)
		if err != nil {
			log.Printf("postgres[%s]: json.Marshal failed for field %q: %v", p.tableRestaurant, field, err)
			return []byte("null")
		}
		return b
	}

	canonicalCID, err := p.canonicalCID(ctx, entry)
	if err != nil {
		atomic.AddInt64(&p.failed, 1)
		log.Printf("postgres[%s]: canonical lookup failed for %q: %v", p.tableRestaurant, entry.Cid, err)
		return fmt.Errorf("canonical restaurant lookup %q: %w", entry.Cid, err)
	}

	qRestaurant := quotePostgresIdent(p.tableRestaurant)
	upsertSQL := fmt.Sprintf(`
INSERT INTO %s (
	cid, input_id, link, title, categories, category, address,
	open_hours, popular_times, web_site, phone, plus_code,
	review_count, review_rating, reviews_per_rating, latitude, longitude,
	status, description, reviews_link, thumbnail, timezone, price_range,
	data_id, place_id, images, reservations, order_online, menu, owner,
	complete_address, about, emails, review_tags
) VALUES (
	$1,$2,$3,$4,$5,$6,$7,
	$8,$9,$10,$11,$12,
	$13,$14,$15,$16,$17,
	$18,$19,$20,$21,$22,$23,
	$24,$25,$26,$27,$28,$29,$30,
	$31,$32,$33,$34
) ON CONFLICT (cid) DO UPDATE SET
	input_id           = EXCLUDED.input_id,
	link               = EXCLUDED.link,
	title              = EXCLUDED.title,
	categories         = EXCLUDED.categories,
	category           = EXCLUDED.category,
	address            = EXCLUDED.address,
	open_hours         = EXCLUDED.open_hours,
	popular_times      = EXCLUDED.popular_times,
	web_site           = EXCLUDED.web_site,
	phone              = EXCLUDED.phone,
	plus_code          = EXCLUDED.plus_code,
	review_count       = EXCLUDED.review_count,
	review_rating      = EXCLUDED.review_rating,
	reviews_per_rating = EXCLUDED.reviews_per_rating,
	latitude           = EXCLUDED.latitude,
	longitude          = EXCLUDED.longitude,
	status             = EXCLUDED.status,
	description        = EXCLUDED.description,
	reviews_link       = EXCLUDED.reviews_link,
	thumbnail          = EXCLUDED.thumbnail,
	timezone           = EXCLUDED.timezone,
	price_range        = EXCLUDED.price_range,
	data_id            = EXCLUDED.data_id,
	place_id           = EXCLUDED.place_id,
	images             = EXCLUDED.images,
	reservations       = EXCLUDED.reservations,
	order_online       = EXCLUDED.order_online,
	menu               = EXCLUDED.menu,
	owner              = EXCLUDED.owner,
	complete_address   = EXCLUDED.complete_address,
	about              = EXCLUDED.about,
	emails             = EXCLUDED.emails,
	review_tags        = EXCLUDED.review_tags,
	updated_at         = NOW(),
	closed_at = CASE
		WHEN EXCLUDED.status ILIKE '%%permanently closed%%'
		THEN COALESCE(%s.closed_at, NOW())
		ELSE %s.closed_at
	END`, qRestaurant, qRestaurant, qRestaurant)

	_, err = p.pool.Exec(ctx, upsertSQL,
		canonicalCID,
		entry.ID,
		entry.Link,
		entry.Title,
		toJSON("categories", entry.Categories),
		entry.Category,
		entry.Address,
		toJSON("open_hours", entry.OpenHours),
		toJSON("popular_times", entry.PopularTimes),
		entry.WebSite,
		entry.Phone,
		entry.PlusCode,
		entry.ReviewCount,
		entry.ReviewRating,
		toJSON("reviews_per_rating", entry.ReviewsPerRating),
		entry.Latitude,
		entry.Longitude,
		entry.Status,
		entry.Description,
		entry.ReviewsLink,
		entry.Thumbnail,
		entry.Timezone,
		entry.PriceRange,
		entry.DataID,
		entry.PlaceID,
		toJSON("images", entry.Images),
		toJSON("reservations", entry.Reservations),
		toJSON("order_online", entry.OrderOnline),
		toJSON("menu", entry.Menu),
		toJSON("owner", entry.Owner),
		toJSON("complete_address", entry.CompleteAddress),
		toJSON("about", entry.About),
		entry.Emails,
		toJSON("review_tags", entry.ReviewTags),
	)
	if err != nil {
		atomic.AddInt64(&p.failed, 1)
		log.Printf("postgres[%s]: write failed for %q: %v", p.tableRestaurant, canonicalCID, err)
		return fmt.Errorf("upsert restaurant %q: %w", canonicalCID, err)
	}

	insertReviewSQL := fmt.Sprintf(`
INSERT INTO %s (
	cid, reviewer_name, profile_picture, rating, description, images, reviewed_at
) VALUES ($1,$2,$3,$4,$5,$6,$7)
ON CONFLICT (cid, reviewer_name) DO NOTHING`, quotePostgresIdent(p.tableReview))

	for _, r := range entry.UserReviews {
		reviewedAt := parseReviewDate(r.When)
		_, err := p.pool.Exec(ctx, insertReviewSQL,
			canonicalCID,
			r.Name,
			r.ProfilePicture,
			r.Rating,
			r.Description,
			toJSON("review_images", r.Images),
			reviewedAt,
		)
		if err != nil {
			atomic.AddInt64(&p.failed, 1)
			log.Printf("postgres[%s]: write failed for %q: %v", p.tableRestaurant, canonicalCID, err)
			return fmt.Errorf("insert review for %q: %w", canonicalCID, err)
		}
	}

	written := atomic.AddInt64(&p.written, 1)
	if written%10 == 0 {
		log.Printf("postgres[%s]: %d records written", p.tableRestaurant, written)
	}

	return nil
}

func (p *PostgresWriter) canonicalCID(ctx context.Context, entry *gmaps.Entry) (string, error) {
	query := fmt.Sprintf(`
SELECT cid
FROM %s
WHERE ($1 <> '' AND cid = $1)
	OR ($2 <> '' AND place_id = $2)
	OR ($3 <> '' AND data_id = $3)
ORDER BY CASE
	WHEN $1 <> '' AND cid = $1 THEN 1
	WHEN $2 <> '' AND place_id = $2 THEN 2
	WHEN $3 <> '' AND data_id = $3 THEN 3
	ELSE 4
END
LIMIT 1`, quotePostgresIdent(p.tableRestaurant))

	var cid string
	if err := p.pool.QueryRow(ctx, query, entry.Cid, entry.PlaceID, entry.DataID).Scan(&cid); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return entry.Cid, nil
		}
		return "", err
	}

	return cid, nil
}

// parseReviewDate parses a "YYYY-M-D" review date into time.Time.
// Returns nil (NULL in Postgres) if the string is empty or unparseable.
func parseReviewDate(when string) *time.Time {
	if when == "" {
		return nil
	}
	parts := strings.Split(when, "-")
	if len(parts) != 3 {
		return nil
	}
	t, err := time.Parse("2006-1-2", when)
	if err != nil {
		return nil
	}
	return &t
}

func (p *PostgresWriter) Flush() error { return nil }

func (p *PostgresWriter) Close() error {
	log.Printf(
		"postgres[%s]: closing — total written=%d failed=%d",
		p.tableRestaurant,
		atomic.LoadInt64(&p.written),
		atomic.LoadInt64(&p.failed),
	)
	p.pool.Close()
	return nil
}

var _ Writer = (*PostgresWriter)(nil)
