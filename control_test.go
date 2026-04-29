package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gosom/google-maps-scraper-lite/gmaps"
)

func TestControlPauseEndpoint(t *testing.T) {
	store, err := gmaps.OpenJobStore(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	jobID, err := store.CreateJob(context.Background(), []string{"coffee"}, nil, []string{"u1"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/"+jobID+"/pause", nil)
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	registerControlHandlers(mux, store, func(ctx context.Context, jobID string) error { return nil })
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	paused, err := store.PauseRequested(context.Background(), jobID)
	if err != nil {
		t.Fatalf("pause requested: %v", err)
	}
	if !paused {
		t.Fatal("pause flag was not set")
	}
}

func TestControlIndexListsJobs(t *testing.T) {
	store, err := gmaps.OpenJobStore(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	jobID, err := store.CreateJob(context.Background(), []string{"coffee"}, nil, []string{"u1"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	registerControlHandlers(mux, store, func(ctx context.Context, jobID string) error { return nil })
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), jobID) {
		t.Fatalf("index did not include job ID %s: %s", jobID, rec.Body.String())
	}
}

func TestControlResumeEndpointLaunchesJob(t *testing.T) {
	store, err := gmaps.OpenJobStore(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	jobID, err := store.CreateJob(context.Background(), []string{"coffee"}, nil, []string{"u1"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := store.RequestPause(context.Background(), jobID); err != nil {
		t.Fatalf("pause job: %v", err)
	}

	var launched string
	req := httptest.NewRequest(http.MethodPost, "/api/jobs/"+jobID+"/resume", nil)
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	registerControlHandlers(mux, store, func(ctx context.Context, id string) error {
		launched = id
		return store.ClearPause(ctx, id)
	})
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if launched != jobID {
		t.Fatalf("launched job = %q, want %q", launched, jobID)
	}
	paused, err := store.PauseRequested(context.Background(), jobID)
	if err != nil {
		t.Fatalf("pause requested: %v", err)
	}
	if paused {
		t.Fatal("resume did not clear pause flag")
	}
}

func TestControlResumeEndpointConflict(t *testing.T) {
	store, err := gmaps.OpenJobStore(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	jobID, err := store.CreateJob(context.Background(), []string{"coffee"}, nil, []string{"u1"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/"+jobID+"/resume", nil)
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	registerControlHandlers(mux, store, func(ctx context.Context, id string) error {
		return errHTTP("job is already running or done")
	})
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
}
