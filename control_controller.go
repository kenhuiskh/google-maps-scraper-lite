package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gosom/google-maps-scraper-lite/gmaps"
)

func startControlServer(ctx context.Context, addr string, store *gmaps.JobStore, stateDB string, launchResume resumeLauncher, launchStart startLauncher) (*http.Server, error) {
	username := os.Getenv("CONTROL_USERNAME")
	password := os.Getenv("CONTROL_PASSWORD")

	mux := http.NewServeMux()
	registerControlHandlers(mux, store, stateDB, launchResume, launchStart)
	if launchStart != nil {
		go runJobQueue(ctx, store, stateDB, launchStart, 30*time.Second)
	}

	var handler http.Handler = mux
	if username == "" || password == "" {
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "control UI is not configured: CONTROL_USERNAME and CONTROL_PASSWORD must both be set", http.StatusServiceUnavailable)
		})
	} else {
		handler = basicAuthMiddleware(username, password, mux)
	}

	srv := &http.Server{Addr: addr, Handler: handler}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	go func() {
		log.Printf("control UI listening on http://%s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("control server error: %v", err)
		}
	}()
	return srv, nil
}

func basicAuthMiddleware(username, password string, next http.Handler) http.Handler {
	usernameHash := sha256.Sum256([]byte(username))
	passwordHash := sha256.Sum256([]byte(password))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		uHash := sha256.Sum256([]byte(u))
		pHash := sha256.Sum256([]byte(p))
		uMatch := subtle.ConstantTimeCompare(uHash[:], usernameHash[:]) == 1
		pMatch := subtle.ConstantTimeCompare(pHash[:], passwordHash[:]) == 1
		if !ok || !uMatch || !pMatch {
			w.Header().Set("WWW-Authenticate", `Basic realm="Scraper Control"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func registerControlHandlers(mux *http.ServeMux, store *gmaps.JobStore, stateDB string, launchResume resumeLauncher, launchStart startLauncher) {
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		jobs, err := store.ListJobs(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		page, pageSize, filter := parseJobsQueueParams(r, "jobs_page", "jobs_page_size", "jobs_filter")
		totalJobs, err := store.CountJobsFiltered(r.Context(), filter)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		pagination := newJobsPagination(page, pageSize, totalJobs, filter)
		pageJobs, err := store.ListJobsPageFiltered(r.Context(), filter, pagination.PageSize, pagination.Offset)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		templates, err := store.ListJobTemplates(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = controlTemplate.ExecuteTemplate(w, "control", newControlPageDataWithPagination(jobs, pageJobs, templates, pagination))
	})
	mux.HandleFunc("/ui/summary", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		jobs, err := store.ListJobs(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = controlTemplate.ExecuteTemplate(w, "summary", newControlPageData(jobs, nil))
	})
	mux.HandleFunc("/ui/jobs", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		jobs, err := store.ListJobs(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		page, pageSize, filter := parseJobsQueueParams(r, "page", "page_size", "filter")
		totalJobs, err := store.CountJobsFiltered(r.Context(), filter)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		pagination := newJobsPagination(page, pageSize, totalJobs, filter)
		pageJobs, err := store.ListJobsPageFiltered(r.Context(), filter, pagination.PageSize, pagination.Offset)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = controlTemplate.ExecuteTemplate(w, "jobs-panel", newControlPageDataWithPagination(jobs, pageJobs, nil, pagination))
	})
	mux.HandleFunc("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			jobs, err := store.ListJobs(r.Context())
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, jobs)
			return
		}
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	})
	mux.HandleFunc("/api/job-templates", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		templates, err := store.ListJobTemplates(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, templates)
	})
	mux.HandleFunc("/api/jobs/start", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if launchStart == nil {
			http.Error(w, "start launcher is not configured", http.StatusServiceUnavailable)
			return
		}
		params, errMsg := parseStartParams(r)
		if errMsg != "" {
			http.Error(w, errMsg, http.StatusBadRequest)
			return
		}
		if params.OutputMode == "file" && params.OutDir == "" {
			params.OutDir = defaultControlOutDir(stateDB)
		}
		jobID, err := store.CreateStartingJob(r.Context(), params.Queries, scraperConfigFromStartParams(params))
		if errors.Is(err, gmaps.ErrActiveJobExists) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		params.JobID = jobID
		templateParams := templateParamsFromForm(r, params)
		if _, err := store.SaveJobTemplate(r.Context(), jobTemplateName(params), templateParams); err != nil {
			log.Printf("save job template: %v", err)
		}
		job, err := store.GetJob(r.Context(), jobID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		status := "queued"
		if job.Status == gmaps.JobStatusStarting {
			if err := launchStart(r.Context(), params); err != nil {
				_ = store.SetJobStatus(context.Background(), jobID, gmaps.JobStatusFailed, err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			status = "started"
		}
		writeJSON(w, map[string]string{"status": status, "job_id": jobID})
	})
	mux.HandleFunc("/api/jobs/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/jobs/")
		parts := strings.Split(strings.Trim(path, "/"), "/")
		if len(parts) == 0 || parts[0] == "" {
			http.NotFound(w, r)
			return
		}
		jobID := parts[0]
		if len(parts) == 1 && r.Method == http.MethodGet {
			job, err := store.GetJob(r.Context(), jobID)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			writeJSON(w, job)
			return
		}
		if len(parts) != 2 || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		switch parts[1] {
		case "pause":
			if err := store.RequestPause(r.Context(), jobID); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, map[string]string{"status": "pause_requested"})
		case "resume":
			if launchResume == nil {
				http.Error(w, "resume launcher is not configured", http.StatusServiceUnavailable)
				return
			}
			if err := launchResume(r.Context(), jobID); err != nil {
				if _, ok := err.(errHTTP); ok {
					http.Error(w, err.Error(), http.StatusConflict)
					return
				}
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, map[string]string{"status": "resume_started"})
		default:
			http.NotFound(w, r)
		}
	})
}

func parseJobsQueueParams(r *http.Request, pageKey, pageSizeKey, filterKey string) (int, int, string) {
	page := 1
	pageSize := defaultJobsPageSize
	filter := defaultJobsFilter
	if raw := strings.TrimSpace(r.URL.Query().Get(pageKey)); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			page = n
		}
	}
	if raw := strings.TrimSpace(r.URL.Query().Get(pageSizeKey)); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			pageSize = n
		}
	}
	if raw := strings.TrimSpace(r.URL.Query().Get(filterKey)); raw != "" {
		filter = normalizeJobsFilter(raw)
	}
	return page, pageSize, filter
}
