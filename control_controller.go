package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
	mux.HandleFunc("/templates/editor", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/templates/editor" {
			http.NotFound(w, r)
			return
		}
		renderControlPage(w, r, store, "template-editor")
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
		if err := controlTemplate.ExecuteTemplate(w, "summary", newControlPageData(jobs, nil, nil)); err != nil {
			log.Printf("template render error (summary): %v", err)
		}
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
		if err := controlTemplate.ExecuteTemplate(w, "jobs-panel", newControlPageDataWithPagination(jobs, pageJobs, nil, nil, pagination)); err != nil {
			log.Printf("template render error (jobs-panel): %v", err)
		}
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
	mux.HandleFunc("/api/config/export", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		templateIDs, strategyIDs := parseConfigExportSelection(r)
		cfg, err := store.ExportReusableConfigSelection(r.Context(), templateIDs, strategyIDs)
		if err != nil {
			if errors.Is(err, gmaps.ErrConfigExportInvalid) {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		filename := "scraper-config-" + time.Now().UTC().Format("20060102-150405") + ".json"
		w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
		writeJSON(w, cfg)
	})
	mux.HandleFunc("/api/config/import", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		mode := gmaps.ConfigImportMode(strings.TrimSpace(r.URL.Query().Get("collision")))
		if mode == "" {
			mode = gmaps.ConfigImportRename
		}
		var cfg gmaps.ReusableConfigExport
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&cfg); err != nil {
			http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
			return
		}
		summary, err := store.ImportReusableConfig(r.Context(), cfg, mode)
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, gmaps.ErrConfigImportInvalid) {
				status = http.StatusBadRequest
			} else if errors.Is(err, gmaps.ErrConfigImportConflict) {
				status = http.StatusConflict
			}
			http.Error(w, err.Error(), status)
			return
		}
		writeJSON(w, summary)
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
		if len(parts) == 2 && parts[1] == "bulk-update-lang" && r.Method == http.MethodPost {
			var req struct {
				Lang       string `json:"lang"`
				Override   bool   `json:"override"`
				NameSuffix string `json:"nameSuffix"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "invalid request body", http.StatusBadRequest)
				return
			}
			req.Lang = strings.TrimSpace(req.Lang)
			if req.Lang == "" {
				http.Error(w, "lang is required", http.StatusBadRequest)
				return
			}
			if !req.Override {
				req.NameSuffix = strings.TrimSpace(req.NameSuffix)
				if req.NameSuffix == "" {
					http.Error(w, "nameSuffix is required when override is false", http.StatusBadRequest)
					return
				}
			}
			var count int
			var err error
			if req.Override {
				count, err = store.BulkUpdateStrategyLang(r.Context(), strategyID, req.Lang)
			} else {
				count, err = store.BulkDuplicateStrategyTemplatesWithLang(r.Context(), strategyID, req.Lang, req.NameSuffix)
			}
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					http.Error(w, "strategy not found", http.StatusNotFound)
					return
				}
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, map[string]string{
				"status":  "ok",
				"message": strconv.Itoa(count) + " template(s) updated.",
			})
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
		if paramsJSON := strings.TrimSpace(r.FormValue("params_json")); paramsJSON != "" {
			if !json.Valid([]byte(paramsJSON)) {
				http.Error(w, "template params must be valid JSON", http.StatusBadRequest)
				return
			}
			templateID, err := store.SaveJobTemplateJSON(r.Context(), params.TemplateID, jobTemplateName(params), paramsJSON)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			params.TemplateID = templateID
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
		if params.TemplateID == "" {
			templateParams := templateParamsFromForm(r, params)
			if _, err := store.SaveJobTemplate(r.Context(), jobTemplateName(params), templateParams); err != nil {
				log.Printf("save job template: %v", err)
			}
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
		if len(parts) == 2 && parts[1] == "logs" && r.Method == http.MethodGet {
			logs, err := jobLogsResponseFromRequest(r, store, stateDB, jobID)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					http.Error(w, err.Error(), http.StatusNotFound)
					return
				}
				if errors.Is(err, os.ErrNotExist) {
					http.Error(w, "log file is not available", http.StatusNotFound)
					return
				}
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, logs)
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
		case "start-pending":
			if launchStart == nil {
				http.Error(w, "start launcher is not configured", http.StatusServiceUnavailable)
				return
			}
			job, err := store.ClaimPendingJob(r.Context(), jobID)
			if err != nil {
				if errors.Is(err, gmaps.ErrActiveJobExists) || errors.Is(err, gmaps.ErrJobNotPending) {
					http.Error(w, err.Error(), http.StatusConflict)
					return
				}
				if errors.Is(err, sql.ErrNoRows) {
					http.Error(w, err.Error(), http.StatusNotFound)
					return
				}
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			params, err := startParamsFromJob(job, stateDB)
			if err != nil {
				_ = store.SetJobStatus(context.Background(), jobID, gmaps.JobStatusFailed, err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if err := launchStart(r.Context(), params); err != nil {
				_ = store.SetJobStatus(context.Background(), jobID, gmaps.JobStatusFailed, err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, map[string]string{"status": "started"})
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
		case "recover-stale":
			if err := store.RecoverStaleActiveJob(r.Context(), jobID, errors.New("process stopped before completion")); err != nil {
				if errors.Is(err, gmaps.ErrJobNotStale) {
					http.Error(w, err.Error(), http.StatusConflict)
					return
				}
				if errors.Is(err, sql.ErrNoRows) {
					http.Error(w, err.Error(), http.StatusNotFound)
					return
				}
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, map[string]string{"status": "recovered"})
		default:
			http.NotFound(w, r)
		}
	})
}

type jobLogsResponse struct {
	JobID     string   `json:"job_id"`
	Available bool     `json:"available"`
	Active    bool     `json:"active"`
	Lines     []string `json:"lines"`
	Message   string   `json:"message,omitempty"`
}

func jobLogsResponseFromRequest(r *http.Request, store *gmaps.JobStore, stateDB, jobID string) (jobLogsResponse, error) {
	job, err := store.GetJob(r.Context(), jobID)
	if err != nil {
		return jobLogsResponse{}, err
	}
	active := isActiveJobStatus(job.Status)
	tail := parseLogTail(r)
	path := jobLogPath(stateDB, jobID)
	response := jobLogsResponse{JobID: jobID, Active: active, Lines: []string{}}
	if path == "" {
		return jobLogsResponse{}, os.ErrNotExist
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return jobLogsResponse{}, os.ErrNotExist
		}
		return jobLogsResponse{}, err
	}
	lines, err := readTailLines(path, tail)
	if err != nil {
		return jobLogsResponse{}, err
	}
	response.Available = true
	response.Lines = lines
	return response, nil
}

func parseLogTail(r *http.Request) int {
	tail := 200
	if raw := strings.TrimSpace(r.URL.Query().Get("tail")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			tail = n
		}
	}
	if tail < 1 {
		return 1
	}
	if tail > 1000 {
		return 1000
	}
	return tail
}

func readTailLines(path string, tail int) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return []string{}, nil
	}
	lines := strings.Split(text, "\n")
	if len(lines) <= tail {
		return lines, nil
	}
	return lines[len(lines)-tail:], nil
}

func isActiveJobStatus(status string) bool {
	return status == gmaps.JobStatusStarting || status == gmaps.JobStatusRunning
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
	editor := newTemplateEditorView()
	switch page {
	case "templates":
		title = "Job Templates"
	case "template-editor":
		var err error
		editor, err = templateEditorViewFromRequest(r, store)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		title = editor.Title
	case "strategies":
		title = "Strategy Management"
	default:
		page = "jobs"
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := newControlPageDataWithPagination(jobs, pageJobs, templates, strategies, pagination).WithPage(page, title).WithTemplateEditor(editor)
	if err := controlTemplate.ExecuteTemplate(w, "control", data); err != nil {
		log.Printf("template render error (control): %v", err)
	}
}

func newTemplateEditorView() templateEditorView {
	return templateEditorView{
		Mode:               "create",
		Title:              "Create Job Template",
		Subtitle:           "Create a template and start a job from the same synced values.",
		CreateTitle:        "Create Job",
		CreateSubtitle:     "Creates or reuses the corresponding template.",
		CreateButton:       "Create Job + Template",
		TemplateParamsJSON: "{\n  \"Queries\": []\n}",
	}
}

func templateEditorViewFromRequest(r *http.Request, store *gmaps.JobStore) (templateEditorView, error) {
	editor := newTemplateEditorView()
	templateID := strings.TrimSpace(r.URL.Query().Get("template_id"))
	if templateID == "" {
		return editor, nil
	}
	tpl, err := store.GetJobTemplate(r.Context(), templateID)
	if err != nil {
		return templateEditorView{}, err
	}
	editor.Mode = "edit"
	editor.Title = "Edit Job Template"
	editor.Subtitle = "Edit the saved template and start jobs from the synced values."
	editor.CreateTitle = "Create Job From Template"
	editor.CreateSubtitle = "Starts a new job using this template's current values."
	editor.CreateButton = "Create Job From Template"
	editor.TemplateID = tpl.ID
	editor.TemplateName = tpl.Name
	editor.TemplateParamsJSON = formatTemplateParamsJSON(tpl.ParamsJSON)
	if r.URL.Query().Get("mode") == "copy" {
		editor.Mode = "copy"
		editor.Title = "Duplicate Job Template"
		editor.Subtitle = "Use an existing template as the starting point for a new one."
		editor.CreateTitle = "Create Job From Copy"
		editor.CreateSubtitle = "Starts a new job using the copied template values."
		editor.CreateButton = "Create Job + Template Copy"
		editor.TemplateID = ""
		editor.TemplateName = strings.TrimSpace(tpl.Name + " copy")
	}
	return editor, nil
}

func formatTemplateParamsJSON(raw string) string {
	var data any
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return raw
	}
	formatted, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return raw
	}
	return string(formatted)
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

func parseConfigExportSelection(r *http.Request) (templateIDs, strategyIDs []string) {
	q := r.URL.Query()
	templateIDs = splitQueryIDs(q["template_id"])
	strategyIDs = splitQueryIDs(q["strategy_id"])
	return templateIDs, strategyIDs
}

func splitQueryIDs(values []string) []string {
	var ids []string
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				ids = append(ids, part)
			}
		}
	}
	return ids
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
	type plannedStrategyJob struct {
		params startParams
		create gmaps.StrategyJobCreate
	}
	planned := make([]plannedStrategyJob, 0, len(strategy.Templates))
	for _, tpl := range strategy.Templates {
		params, err := startParamsFromTemplate(tpl, stateDB)
		if err != nil {
			return strategyRunResponse{}, fmt.Errorf("template %d %q (%s): %w", len(planned)+1, tpl.Name, tpl.ID, err)
		}
		params.StrategyID = strategy.ID
		params.StrategyRunID = runID
		planned = append(planned, plannedStrategyJob{
			params: params,
			create: gmaps.StrategyJobCreate{
				Queries:       params.Queries,
				Config:        scraperConfigFromStartParams(params),
				TemplateID:    tpl.ID,
				StrategyID:    strategy.ID,
				StrategyRunID: runID,
			},
		})
	}
	creates := make([]gmaps.StrategyJobCreate, 0, len(planned))
	for _, job := range planned {
		creates = append(creates, job.create)
	}
	jobIDs, startedID, err := store.CreateStrategyJobsWithSource(ctx, creates)
	if err != nil {
		return strategyRunResponse{}, err
	}
	response.JobIDs = append(response.JobIDs, jobIDs...)
	if startedID != "" {
		response.StartedJobID = startedID
		response.Status = "started"
		for i, jobID := range jobIDs {
			if jobID != startedID {
				continue
			}
			params := planned[i].params
			params.JobID = jobID
			if err := launchStart(ctx, params); err != nil {
				_ = store.SetJobStatus(context.Background(), jobID, gmaps.JobStatusFailed, err)
				return strategyRunResponse{}, err
			}
			break
		}
	}
	_ = store.MarkStrategyUsed(ctx, strategy.ID)
	return response, nil
}
