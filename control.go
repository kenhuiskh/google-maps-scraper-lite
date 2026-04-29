package main

import (
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/gosom/google-maps-scraper-lite/gmaps"
)

type resumeLauncher func(ctx context.Context, jobID string) error

func startControlServer(ctx context.Context, addr string, store *gmaps.JobStore, launchResume resumeLauncher) (*http.Server, error) {
	mux := http.NewServeMux()
	registerControlHandlers(mux, store, launchResume)
	srv := &http.Server{Addr: addr, Handler: mux}
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

func registerControlHandlers(mux *http.ServeMux, store *gmaps.JobStore, launchResume resumeLauncher) {
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
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = controlTemplate.Execute(w, jobs)
	})
	mux.HandleFunc("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		jobs, err := store.ListJobs(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, jobs)
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

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

var controlTemplate = template.Must(template.New("control").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Scraper Jobs</title>
  <style>
    body { font-family: system-ui, -apple-system, sans-serif; margin: 32px; color: #17202a; }
    table { border-collapse: collapse; width: 100%; }
    th, td { border-bottom: 1px solid #d8dee4; padding: 8px; text-align: left; }
    button { padding: 6px 10px; }
    .muted { color: #667085; }
  </style>
</head>
<body>
  <h1>Scraper Jobs</h1>
  <table>
    <thead><tr><th>Job</th><th>Status</th><th>Progress</th><th>Pause</th><th>Actions</th></tr></thead>
    <tbody>
    {{range .}}
      <tr>
        <td><code>{{.ID}}</code></td>
        <td>{{.Status}}</td>
        <td>{{.Stats.Done}} / {{.Stats.Total}} done, {{.Stats.Failed}} failed</td>
        <td>{{.PauseRequested}}</td>
        <td>
          <button onclick="post('/api/jobs/{{.ID}}/pause')">Pause</button>
          <button onclick="post('/api/jobs/{{.ID}}/resume')">Resume</button>
        </td>
      </tr>
    {{else}}
      <tr><td colspan="5" class="muted">No jobs yet.</td></tr>
    {{end}}
    </tbody>
  </table>
  <script>
    async function post(path) {
      await fetch(path, { method: 'POST' });
      location.reload();
    }
  </script>
</body>
</html>`))

func newProcessResumeLauncher(store *gmaps.JobStore, stateDB string) resumeLauncher {
	return func(ctx context.Context, jobID string) error {
		job, err := store.ClaimResume(ctx, jobID)
		if errors.Is(err, gmaps.ErrJobNotResumable) {
			return errHTTP(err.Error())
		}
		if err != nil {
			return err
		}
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		args := buildResumeArgs(job, stateDB)
		cmd := exec.CommandContext(context.Background(), exe, args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Stdin = nil
		cmd.Env = os.Environ()
		if err := cmd.Start(); err != nil {
			_ = store.SetJobStatus(context.Background(), jobID, gmaps.JobStatusFailed, err)
			return err
		}
		go func() {
			if err := cmd.Wait(); err != nil {
				log.Printf("resume process for %s exited: %v", jobID, err)
			}
		}()
		log.Printf("started resume process for %s: %s %s", jobID, exe, strings.Join(args, " "))
		return nil
	}
}

func buildResumeArgs(job *gmaps.Job, stateDB string) []string {
	args := []string{"-job", job.ID, "-state-db", stateDB}
	var cfg gmaps.Config
	if err := json.Unmarshal([]byte(job.ConfigJSON), &cfg); err != nil {
		return args
	}
	if cfg.Concurrency > 0 {
		args = append(args, "-c", strconv.Itoa(cfg.Concurrency))
	}
	if cfg.Depth > 0 {
		args = append(args, "-depth", strconv.Itoa(cfg.Depth))
	}
	if cfg.Lang != "" {
		args = append(args, "-lang", cfg.Lang)
	}
	if cfg.Geo != "" {
		args = append(args, "-geo", cfg.Geo)
	}
	if cfg.Radius > 0 {
		args = append(args, "-radius", strconv.FormatFloat(cfg.Radius, 'f', -1, 64))
	}
	if cfg.ExtractEmail {
		args = append(args, "-email")
	}
	if cfg.ExtraReviews > 0 {
		args = append(args, "-reviews", strconv.Itoa(cfg.ExtraReviews))
	}
	return args
}

type errHTTP string

func (e errHTTP) Error() string { return string(e) }
