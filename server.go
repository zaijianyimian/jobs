package main

import (
	_ "embed"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
)

//go:embed index.html
var indexHTML []byte

func registerRoutes(mux *http.ServeMux, store *Store) {
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":             true,
			"version":        appVersion,
			"schema_version": schemaVersion,
		})
	})

	mux.HandleFunc("GET /api/applications", func(w http.ResponseWriter, _ *http.Request) {
		state := store.stateCopy()
		out := make([]Application, 0, len(state.Applications))
		for _, app := range state.Applications {
			out = append(out, enrich(store, app))
		}
		writeJSON(w, http.StatusOK, out)
	})

	mux.HandleFunc("POST /api/applications", func(w http.ResponseWriter, r *http.Request) {
		app, err := decodeMap(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		created, err := store.createApplication(app)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusCreated, enrich(store, created))
	})

	mux.HandleFunc("GET /api/applications/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			http.Error(w, "invalid id", http.StatusBadRequest)
			return
		}
		app, ok := store.application(id)
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, enrich(store, app))
	})

	mux.HandleFunc("PUT /api/applications/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			http.Error(w, "invalid id", http.StatusBadRequest)
			return
		}
		patch, err := decodeMap(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		app, err := store.updateApplication(id, patch)
		if errors.Is(err, os.ErrNotExist) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, enrich(store, app))
	})

	mux.HandleFunc("DELETE /api/applications/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			http.Error(w, "invalid id", http.StatusBadRequest)
			return
		}
		if err := store.deleteApplication(id); errors.Is(err, os.ErrNotExist) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		} else if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("POST /api/applications/{id}/official-check", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			http.Error(w, "invalid id", http.StatusBadRequest)
			return
		}
		patch, err := decodeMap(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		app, err := store.recordOfficialCheck(id, patch)
		if errors.Is(err, os.ErrNotExist) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, enrich(store, app))
	})

	mux.HandleFunc("GET /api/official-checks/due", func(w http.ResponseWriter, _ *http.Request) {
		state := store.stateCopy()
		out := []Application{}
		for _, app := range state.Applications {
			if store.officialCheckDue(app) {
				out = append(out, enrich(store, app))
			}
		}
		writeJSON(w, http.StatusOK, out)
	})

	mux.HandleFunc("GET /api/resume-versions", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, store.stateCopy().ResumeVersions)
	})

	mux.HandleFunc("GET /api/events", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, store.stateCopy().Events)
	})

	mux.HandleFunc("POST /api/events", func(w http.ResponseWriter, r *http.Request) {
		event, err := decodeGenericMap(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		created, err := store.createEvent(event)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusCreated, created)
	})

	mux.HandleFunc("POST /api/events/{id}/toggle", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			http.Error(w, "invalid id", http.StatusBadRequest)
			return
		}
		event, err := store.toggleEvent(id)
		if errors.Is(err, os.ErrNotExist) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, event)
	})

	mux.HandleFunc("GET /api/duplicates", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, duplicateGroups(store.stateCopy().Applications))
	})

	mux.HandleFunc("GET /api/analytics/overview", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, analytics(store.stateCopy()))
	})

	mux.HandleFunc("GET /api/export/gpt", func(w http.ResponseWriter, r *http.Request) {
		ids := parseIDs(r.URL.Query().Get("ids"))
		text := exportGPT(store.stateCopy().Applications, ids)
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Content-Disposition", "attachment; filename=job-tracker-gpt.md")
		_, _ = io.WriteString(w, text)
	})
}

func enrich(store *Store, app Application) Application {
	out := cloneMap(app)
	out["official_check_due"] = store.officialCheckDue(app)
	out["official_check_resolved_url"] = officialURL(app)
	out["terminal"] = isTerminal(app)
	return out
}
