package gmaps

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestNormalizeLangs(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", []string{"en"}},
		{"en", []string{"en"}},
		{"en,fr", []string{"en", "fr"}},
		{" EN , Fr ", []string{"en", "fr"}},
		{"en,en,fr", []string{"en", "fr"}},
		{",,", []string{"en"}},
	}
	for _, tc := range cases {
		got := normalizeLangs(tc.in)
		if len(got) != len(tc.want) {
			t.Fatalf("normalizeLangs(%q) = %#v, want %#v", tc.in, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("normalizeLangs(%q) = %#v, want %#v", tc.in, got, tc.want)
			}
		}
	}
}

func TestExpandLangsURLMajor(t *testing.T) {
	urls := []string{"a", "b"}
	langs := []string{"en", "fr"}
	got := expandLangs(urls, langs)
	want := []QueuedURL{
		{URL: "a", Lang: "en"},
		{URL: "a", Lang: "fr"},
		{URL: "b", Lang: "en"},
		{URL: "b", Lang: "fr"},
	}
	if len(got) != len(want) {
		t.Fatalf("expandLangs len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expandLangs[%d] = %#v, want %#v", i, got[i], want[i])
		}
	}
}

func TestClaimNextURLReturnsLang(t *testing.T) {
	ctx := context.Background()
	store := newTestJobStore(t)

	jobID, err := store.CreateJob(ctx, []string{"coffee"}, nil, []QueuedURL{
		{URL: "https://maps/x", Lang: "fr"},
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := store.StartJob(ctx, jobID); err != nil {
		t.Fatalf("start job: %v", err)
	}
	claimed, err := store.ClaimNextURL(ctx, jobID)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed.Lang != "fr" {
		t.Fatalf("claimed lang = %q, want fr", claimed.Lang)
	}
}

func TestSamePlaceQueuedPerLang(t *testing.T) {
	ctx := context.Background()
	store := newTestJobStore(t)

	// The same canonical URL in two languages must coexist (uniqueness is on
	// (job_id, url, lang)).
	if _, err := store.CreateJob(ctx, []string{"coffee"}, nil, []QueuedURL{
		{URL: "https://maps/x", Lang: "en"},
		{URL: "https://maps/x", Lang: "fr"},
	}); err != nil {
		t.Fatalf("create job with same url two langs: %v", err)
	}
}

// TestMigrateJobURLsLang builds a pre-lang job_urls table, then verifies that
// opening the store rebuilds it with a lang column (legacy rows defaulting to
// ”) and the new (job_id, url, lang) uniqueness.
func TestMigrateJobURLsLang(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.sqlite")

	db, err := sql.Open("sqlite3", path+"?_foreign_keys=on")
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	now := time.Now().UTC()
	legacy := []string{
		`CREATE TABLE jobs (
			id TEXT PRIMARY KEY,
			queries_json TEXT NOT NULL,
			config_json TEXT NOT NULL DEFAULT '{}',
			status TEXT NOT NULL,
			pause_requested INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL,
			started_at DATETIME,
			updated_at DATETIME NOT NULL,
			finished_at DATETIME,
			last_error TEXT
		)`,
		`CREATE TABLE job_urls (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			job_id TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
			position INTEGER NOT NULL,
			url TEXT NOT NULL,
			status TEXT NOT NULL,
			attempts INTEGER NOT NULL DEFAULT 0,
			last_error TEXT,
			started_at DATETIME,
			finished_at DATETIME,
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL,
			UNIQUE(job_id, position),
			UNIQUE(job_id, url)
		)`,
	}
	for _, stmt := range legacy {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("legacy ddl: %v", err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO jobs (id, queries_json, config_json, status, created_at, updated_at) VALUES (?,?,?,?,?,?)`,
		"job-1", `["coffee"]`, `{"Lang":"en"}`, JobStatusPending, now, now); err != nil {
		t.Fatalf("legacy job insert: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO job_urls (job_id, position, url, status, created_at, updated_at) VALUES (?,?,?,?,?,?)`,
		"job-1", 0, "https://maps/old", URLStatusDone, now, now); err != nil {
		t.Fatalf("legacy url insert: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	store, err := OpenJobStore(path)
	if err != nil {
		t.Fatalf("open store (migrate): %v", err)
	}
	defer store.Close()

	has, err := store.hasColumn(ctx, "job_urls", "lang")
	if err != nil || !has {
		t.Fatalf("lang column present = %v (err %v)", has, err)
	}

	var url, lang string
	if err := store.db.QueryRowContext(ctx, `SELECT url, lang FROM job_urls WHERE job_id='job-1'`).Scan(&url, &lang); err != nil {
		t.Fatalf("read migrated row: %v", err)
	}
	if url != "https://maps/old" || lang != "" {
		t.Fatalf("migrated row = (%q,%q), want (https://maps/old,'')", url, lang)
	}

	// Same url, different lang must be insertable post-migration.
	if _, err := store.db.ExecContext(ctx, `INSERT INTO job_urls (job_id, position, url, lang, status, created_at, updated_at) VALUES (?,?,?,?,?,?,?)`,
		"job-1", 1, "https://maps/old", "fr", URLStatusPending, now, now); err != nil {
		t.Fatalf("insert same url different lang: %v", err)
	}
}
