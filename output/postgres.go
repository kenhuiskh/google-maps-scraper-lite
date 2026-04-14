package output

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gosom/google-maps-scraper-lite/gmaps"
	"github.com/jackc/pgx/v5/pgxpool"
)

const createRestaurantsSQL = `
CREATE TABLE IF NOT EXISTS restaurants (
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

const createReviewsSQL = `
CREATE TABLE IF NOT EXISTS restaurant_reviews (
	id              BIGSERIAL   PRIMARY KEY,
	cid             TEXT        NOT NULL REFERENCES restaurants(cid) ON DELETE CASCADE,
	reviewer_name   TEXT,
	profile_picture TEXT,
	rating          INTEGER,
	description     TEXT,
	images          JSONB,
	reviewed_at     DATE,
	created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	UNIQUE (cid, reviewer_name, reviewed_at)
)`

const upsertRestaurantSQL = `
INSERT INTO restaurants (
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
		WHEN EXCLUDED.status ILIKE '%permanently closed%'
		THEN COALESCE(restaurants.closed_at, NOW())
		ELSE restaurants.closed_at
	END`

const insertReviewSQL = `
INSERT INTO restaurant_reviews (
	cid, reviewer_name, profile_picture, rating, description, images, reviewed_at
) VALUES ($1,$2,$3,$4,$5,$6,$7)
ON CONFLICT (cid, reviewer_name, reviewed_at) DO NOTHING`

type PostgresWriter struct {
	pool    *pgxpool.Pool
	written int64
	failed  int64
}

func NewPostgresWriter(ctx context.Context, dsn string) (*PostgresWriter, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect to postgres: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	if _, err := pool.Exec(ctx, createRestaurantsSQL); err != nil {
		pool.Close()
		return nil, fmt.Errorf("create restaurants table: %w", err)
	}

	if _, err := pool.Exec(ctx, createReviewsSQL); err != nil {
		pool.Close()
		return nil, fmt.Errorf("create restaurant_reviews table: %w", err)
	}

	return &PostgresWriter{pool: pool}, nil
}

func (p *PostgresWriter) Write(entry *gmaps.Entry) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	toJSON := func(field string, v any) []byte {
		b, err := json.Marshal(v)
		if err != nil {
			log.Printf("postgres: json.Marshal failed for field %q: %v", field, err)
			return []byte("null")
		}
		return b
	}

	_, err := p.pool.Exec(ctx, upsertRestaurantSQL,
		entry.Cid,
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
		log.Printf("postgres: write failed for %q: %v", entry.Cid, err)
		return fmt.Errorf("upsert restaurant %q: %w", entry.Cid, err)
	}

	for _, r := range entry.UserReviews {
		reviewedAt := parseReviewDate(r.When)
		_, err := p.pool.Exec(ctx, insertReviewSQL,
			entry.Cid,
			r.Name,
			r.ProfilePicture,
			r.Rating,
			r.Description,
			toJSON("review_images", r.Images),
			reviewedAt,
		)
		if err != nil {
			atomic.AddInt64(&p.failed, 1)
			log.Printf("postgres: write failed for %q: %v", entry.Cid, err)
			return fmt.Errorf("insert review for %q: %w", entry.Cid, err)
		}
	}

	written := atomic.AddInt64(&p.written, 1)
	if written%10 == 0 {
		log.Printf("postgres: %d records written", written)
	}

	return nil
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
		"postgres: closing — total written=%d failed=%d",
		atomic.LoadInt64(&p.written),
		atomic.LoadInt64(&p.failed),
	)
	p.pool.Close()
	return nil
}

var _ Writer = (*PostgresWriter)(nil)
