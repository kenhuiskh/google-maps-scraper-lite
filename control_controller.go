package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
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
		if r.URL.Path != "/" && r.URL.Path != "/jobs" {
			http.NotFound(w, r)
			return
		}
		renderControlPage(w, r, store, "jobs")
	})
	mux.HandleFunc("/templates", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/templates" {
			http.NotFound(w, r)
			return
		}
		renderControlPage(w, r, store, "templates")
	})
	mux.HandleFunc("/strategies", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/strategies" {
			http.NotFound(w, r)
			return
		}
		renderControlPage(w, r, store, "strategies")
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
		_ = controlTemplate.ExecuteTemplate(w, "summary", newControlPageData(jobs, nil, nil))
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
		_ = controlTemplate.ExecuteTemplate(w, "jobs-panel", newControlPageDataWithPagination(jobs, pageJobs, nil, nil, pagination))
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
		switch r.Method {
		case http.MethodGet:
			templates, err := store.ListJobTemplates(r.Context())
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, templates)
		case http.MethodPost:
			id, name, paramsJSON, err := parseTemplateSaveRequest(r)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			savedID, err := store.SaveJobTemplateJSON(r.Context(), id, name, paramsJSON)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			tpl, err := store.GetJobTemplate(r.Context(), savedID)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, tpl)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/job-templates/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/job-templates/"), "/")
		if id == "" {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			tpl, err := store.GetJobTemplate(r.Context(), id)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			writeJSON(w, tpl)
		case http.MethodDelete:
			if err := store.DeleteJobTemplate(r.Context(), id); err != nil {
				status := http.StatusInternalServerError
				if errors.Is(err, sql.ErrNoRows) {
					status = http.StatusNotFound
				} else if strings.Contains(err.Error(), "strategy") {
					status = http.StatusConflict
				}
				http.Error(w, err.Error(), status)
				return
			}
			writeJSON(w, map[string]string{"status": "deleted"})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/strategies", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			strategies, err := store.ListStrategies(r.Context())
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, strategies)
		case http.MethodPost:
			id, name, notes, templateIDs, err := parseStrategySaveRequest(r)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			savedID, err := store.SaveStrategy(r.Context(), id, name, notes, templateIDs)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			strategy, err := store.GetStrategy(r.Context(), savedID)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, strategy)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/strategies/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/strategies/"), "/")
		parts := strings.Split(path, "/")
		if len(parts) == 0 || parts[0] == "" {
			http.NotFound(w, r)
			return
		}
		strategyID := parts[0]
		if len(parts) == 1 {
			switch r.Method {
			case http.MethodGet:
				strategy, err := store.GetStrategy(r.Context(), strategyID)
				if err != nil {
					http.Error(w, err.Error(), http.StatusNotFound)
					return
				}
				writeJSON(w, strategy)
			case http.MethodPut, http.MethodPost:
				_, name, notes, templateIDs, err := parseStrategySaveRequest(r)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				if _, err := store.SaveStrategy(r.Context(), strategyID, name, notes, templateIDs); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				strategy, err := store.GetStrategy(r.Context(), strategyID)
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				writeJSON(w, strategy)
			case http.MethodDelete:
				if err := store.DeleteStrategy(r.Context(), strategyID); err != nil {
					if errors.Is(err, sql.ErrNoRows) {
						http.Error(w, err.Error(), http.StatusNotFound)
						return
					}
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				writeJSON(w, map[string]string{"status": "deleted"})
			default:
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			}
			return
		}
		if len(parts) == 2 && parts[1] == "run" && r.Method == http.MethodPost {
			result, err := runStrategyFromControl(r.Context(), store, stateDB, launchStart, strategyID)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			writeJSON(w, result)
			return
		}
		http.NotFound(w, r)
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
		jobID, err := store.CreateStartingJobWithSource(r.Context(), params.Queries, scraperConfigFromStartParams(params), params.TemplateID, "", "")
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

func renderControlPage(w http.ResponseWriter, r *http.Request, store *gmaps.JobStore, page string) {
	jobs, err := store.ListJobs(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	pageNum, pageSize, filter := parseJobsQueueParams(r, "jobs_page", "jobs_page_size", "jobs_filter")
	totalJobs, err := store.CountJobsFiltered(r.Context(), filter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	pagination := newJobsPagination(pageNum, pageSize, totalJobs, filter)
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
	strategies, err := store.ListStrategies(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	title := "Dashboard"
	switch page {
	case "templates":
		title = "Job Templates"
	case "strategies":
		title = "Strategy Management"
	default:
		page = "jobs"
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := newControlPageDataWithPagination(jobs, pageJobs, templates, strategies, pagination).WithPage(page, title)
	_ = controlTemplate.ExecuteTemplate(w, "control", data)
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

func parseTemplateSaveRequest(r *http.Request) (id, name, paramsJSON string, err error) {
	ct := r.Header.Get("Content-Type")
	if strings.Contains(ct, "application/json") {
		var payload struct {
			ID         string
			Name       string
			ParamsJSON string
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			return "", "", "", err
		}
		return strings.TrimSpace(payload.ID), strings.TrimSpace(payload.Name), strings.TrimSpace(payload.ParamsJSON), nil
	}
	if err := r.ParseForm(); err != nil {
		return "", "", "", err
	}
	return strings.TrimSpace(r.FormValue("id")), strings.TrimSpace(r.FormValue("name")), strings.TrimSpace(r.FormValue("params_json")), nil
}

func parseStrategySaveRequest(r *http.Request) (id, name, notes string, templateIDs []string, err error) {
	ct := r.Header.Get("Content-Type")
	if strings.Contains(ct, "application/json") {
		var payload struct {
			ID          string
			Name        string
			Notes       string
			TemplateIDs []string
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			return "", "", "", nil, err
		}
		return strings.TrimSpace(payload.ID), strings.TrimSpace(payload.Name), strings.TrimSpace(payload.Notes), payload.TemplateIDs, nil
	}
	if err := r.ParseForm(); err != nil {
		return "", "", "", nil, err
	}
	rawIDs := r.Form["template_ids"]
	if len(rawIDs) == 0 {
		raw := strings.TrimSpace(r.FormValue("template_ids"))
		if raw != "" {
			rawIDs = strings.Split(raw, ",")
		}
	}
	return strings.TrimSpace(r.FormValue("id")), strings.TrimSpace(r.FormValue("name")), strings.TrimSpace(r.FormValue("notes")), rawIDs, nil
}

type strategyRunResponse struct {
	Status        string   `json:"status"`
	StrategyRunID string   `json:"strategy_run_id"`
	JobIDs        []string `json:"job_ids"`
	StartedJobID  string   `json:"started_job_id,omitempty"`
}

func runStrategyFromControl(ctx context.Context, store *gmaps.JobStore, stateDB string, launchStart startLauncher, strategyID string) (strategyRunResponse, error) {
	if launchStart == nil {
		return strategyRunResponse{}, errors.New("start launcher is not configured")
	}
	strategy, err := store.GetStrategy(ctx, strategyID)
	if err != nil {
		return strategyRunResponse{}, err
	}
	if len(strategy.Templates) == 0 {
		return strategyRunResponse{}, errors.New("strategy has no templates")
	}
	runID := gmaps.NewStrategyRunID(time.Now().UTC())
	var response strategyRunResponse
	response.Status = "queued"
	response.StrategyRunID = runID
	for _, tpl := range strategy.Templates {
		params, err := startParamsFromTemplate(tpl, stateDB)
		if err != nil {
			return strategyRunResponse{}, err
		}
		params.StrategyID = strategy.ID
		params.StrategyRunID = runID
		jobID, err := store.CreateStartingJobWithSource(ctx, params.Queries, scraperConfigFromStartParams(params), tpl.ID, strategy.ID, runID)
		if err != nil {
			return strategyRunResponse{}, err
		}
		params.JobID = jobID
		response.JobIDs = append(response.JobIDs, jobID)
		job, err := store.GetJob(ctx, jobID)
		if err != nil {
			return strategyRunResponse{}, err
		}
		if job.Status == gmaps.JobStatusStarting && response.StartedJobID == "" {
			if err := launchStart(ctx, params); err != nil {
				_ = store.SetJobStatus(context.Background(), jobID, gmaps.JobStatusFailed, err)
				return strategyRunResponse{}, err
			}
			response.StartedJobID = jobID
			response.Status = "started"
		}
	}
	_ = store.MarkStrategyUsed(ctx, strategy.ID)
	return response, nil
}
