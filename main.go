package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	schemaVersion = 10
	appVersion    = "9.2"
)

//go:embed index.html
var indexHTML []byte

type Application map[string]any

type State struct {
	SchemaVersion  int              `json:"schema_version"`
	AppVersion     string           `json:"app_version"`
	Applications   []Application    `json:"applications"`
	Events         []map[string]any  `json:"events"`
	ResumeVersions []map[string]any  `json:"resume_versions"`
}

type Store struct {
	mu   sync.RWMutex
	path string
	loc  *time.Location
	data State
}

func main() {
	dataPath := env("JOB_TRACKER_DATA", "./data/jobtracker.json")
	addr := env("JOB_TRACKER_ADDR", ":8000")
	tz := env("JOB_TRACKER_TZ", "Asia/Shanghai")

	loc, err := time.LoadLocation(tz)
	if err != nil {
		log.Printf("cannot load timezone %q; falling back to Local: %v", tz, err)
		loc = time.Local
	}

	store, err := NewStore(dataPath, loc)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}

	mux := http.NewServeMux()
	registerRoutes(mux, store)

	server := &http.Server{
		Addr:              addr,
		Handler:           loggingMiddleware(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("job-tracker v%s listening on %s; data=%s; tz=%s", appVersion, addr, dataPath, tz)
	log.Fatal(server.ListenAndServe())
}

func env(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func NewStore(path string, loc *time.Location) (*Store, error) {
	s := &Store{path: path, loc: loc}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) now() time.Time { return time.Now().In(s.loc) }

func (s *Store) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}

	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		s.data = State{
			SchemaVersion:  schemaVersion,
			AppVersion:     appVersion,
			Applications:   []Application{},
			Events:         []map[string]any{},
			ResumeVersions: []map[string]any{},
		}
		return s.saveLocked()
	}
	if err != nil {
		return fmt.Errorf("read data: %w", err)
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&s.data); err != nil {
		return fmt.Errorf("decode %s: %w", s.path, err)
	}

	if s.data.Applications == nil {
		s.data.Applications = []Application{}
	}
	if s.data.Events == nil {
		s.data.Events = []map[string]any{}
	}
	if s.data.ResumeVersions == nil {
		s.data.ResumeVersions = []map[string]any{}
	}

	if s.data.SchemaVersion < schemaVersion {
		backup := fmt.Sprintf("%s.before-v92-%s", s.path, s.now().Format("20060102-150405"))
		if err := os.WriteFile(backup, raw, 0o600); err != nil {
			return fmt.Errorf("backup before migration: %w", err)
		}
		if err := s.migrateLocked(); err != nil {
			return err
		}
		return s.saveLocked()
	}

	s.data.AppVersion = appVersion
	return nil
}

func (s *Store) migrateLocked() error {
	for _, app := range s.data.Applications {
		if isOfficialSource(str(app, "source")) && !isTerminal(app) {
			if _, exists := app["official_check_enabled"]; !exists {
				app["official_check_enabled"] = true
			}
		}
		s.normalizeSchedule(app)
	}
	s.data.SchemaVersion = schemaVersion
	s.data.AppVersion = appVersion
	return nil
}

func (s *Store) normalizeSchedule(app Application) {
	if isTerminal(app) || !boolValue(app, "official_check_enabled") {
		app["next_official_check_at"] = ""
		return
	}
	if str(app, "next_official_check_at") != "" {
		return
	}
	base, ok := s.applicationBaseDate(app)
	if !ok {
		base = s.now()
	}
	app["next_official_check_at"] = base.AddDate(0, 0, 7).Format("2006-01-02")
}

func (s *Store) applicationBaseDate(app Application) (time.Time, bool) {
	for _, key := range []string{"last_official_check_at", "applied", "applied_at"} {
		if t, ok := parseDate(str(app, key), s.loc); ok {
			return t, true
		}
	}
	return time.Time{}, false
}

func parseDate(value string, loc *time.Location) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	if t, err := time.ParseInLocation("2006-01-02", value, loc); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t.In(loc), true
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02T15:04"} {
		if t, err := time.ParseInLocation(layout, value, loc); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func (s *Store) officialCheckDue(app Application) bool {
	if isTerminal(app) || !boolValue(app, "official_check_enabled") {
		return false
	}
	next, ok := parseDate(str(app, "next_official_check_at"), s.loc)
	if !ok {
		return false
	}
	now := s.now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, s.loc)
	return !next.After(today)
}

func (s *Store) saveLocked() error {
	s.data.SchemaVersion = schemaVersion
	s.data.AppVersion = appVersion

	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	raw = append(raw, '\n')

	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".jobtracker-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func (s *Store) stateCopy() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	raw, _ := json.Marshal(s.data)
	var out State
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	_ = decoder.Decode(&out)
	return out
}

func (s *Store) application(id int) (Application, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, app := range s.data.Applications {
		if intValue(app, "id") == id {
			return cloneMap(app), true
		}
	}
	return nil, false
}

func (s *Store) createApplication(app Application) (Application, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if strings.TrimSpace(str(app, "company")) == "" || strings.TrimSpace(str(app, "role")) == "" {
		return nil, errors.New("company and role are required")
	}

	maxID := 0
	for _, item := range s.data.Applications {
		if id := intValue(item, "id"); id > maxID {
			maxID = id
		}
	}
	app["id"] = maxID + 1
	if str(app, "applied") == "" {
		app["applied"] = s.now().Format("2006-01-02")
	}
	if str(app, "applied_at") == "" {
		app["applied_at"] = str(app, "applied")
	}
	if str(app, "current_stage") == "" {
		app["current_stage"] = "已投递"
	}
	if str(app, "current_status") == "" {
		app["current_status"] = str(app, "current_stage")
	}
	if str(app, "highest_stage") == "" {
		app["highest_stage"] = str(app, "current_stage")
	}
	if _, exists := app["official_check_enabled"]; !exists && isOfficialSource(str(app, "source")) {
		app["official_check_enabled"] = true
	}
	s.normalizeSchedule(app)
	s.data.Applications = append(s.data.Applications, app)
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	return cloneMap(app), nil
}

func (s *Store) updateApplication(id int, patch Application) (Application, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, current := range s.data.Applications {
		if intValue(current, "id") != id {
			continue
		}
		for key, value := range patch {
			if key != "id" && !strings.HasPrefix(key, "official_check_due") && key != "terminal" && key != "official_check_resolved_url" {
				current[key] = value
			}
		current["id"] = id
		if _, exists := current["official_check_enabled"]; !exists && isOfficialSource(str(current, "source")) {
			current["official_check_enabled"] = true
		}
		s.normalizeSchedule(current)
		s.data.Applications[i] = current
		if err := s.saveLocked(); err != nil {
			return nil, err
		}
		return cloneMap(current), nil
	}
	return nil, os.ErrNotExist
}

func (s *Store) deleteApplication(id int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, app := range s.data.Applications {
		if intValue(app, "id") != id {
			continue
		}
		s.data.Applications = append(s.data.Applications[:i], s.data.Applications[i+1:]...)
		filtered := make([]map[string]any, 0, len(s.data.Events))
		for _, event := range s.data.Events {
			if intValue(event, "application_id") != id {
				filtered = append(filtered, event)
			}
		}
		s.data.Events = filtered
		return s.saveLocked()
	}
	return os.ErrNotExist
}

func (s *Store) recordOfficialCheck(id int, patch Application) (Application, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, app := range s.data.Applications {
		if intValue(app, "id") != id {
			continue
		}
		for _, key := range []string{"current_stage", "current_status", "outcome", "failure_stage", "official_check_url"} {
			if value, exists := patch[key]; exists {
				app[key] = value
			}
		}
		if note, exists := patch["note"]; exists {
			app["official_check_note"] = note
		}
		app["official_check_enabled"] = true
		app["last_official_check_at"] = s.now().Format("2006-01-02")
		app["official_check_count"] = intValue(app, "official_check_count") + 1
		if isTerminal(app) {
			app["next_official_check_at"] = ""
		} else {
			app["next_official_check_at"] = s.now().AddDate(0, 0, 7).Format("2006-01-02")
		}
		if err := s.saveLocked(); err != nil {
			return nil, err
		}
		return cloneMap(app), nil
	}
	return nil, os.ErrNotExist
}

func (s *Store) createEvent(event map[string]any) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	maxID := 0
	for _, item := range s.data.Events {
		if id := intValue(item, "id"); id > maxID {
			maxID = id
		}
	}
	event["id"] = maxID + 1
	if _, exists := event["completed"]; !exists {
		event["completed"] = false
	}
	s.data.Events = append(s.data.Events, event)
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	return cloneMap(event), nil
}

func (s *Store) toggleEvent(id int) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, event := range s.data.Events {
		if intValue(event, "id") != id {
			continue
		}
		event["completed"] = !boolValue(event, "completed")
		if err := s.saveLocked(); err != nil {
			return nil, err
		}
		return cloneMap(event), nil
	}
	return nil, os.ErrNotExist
}

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
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": appVersion, "schema_version": schemaVersion})
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

func isOfficialSource(source string) bool { return strings.Contains(strings.TrimSpace(source), "官网") }

func isTerminal(app Application) bool {
	if boolValue(app, "archived") {
		return true
	}
	stage := strings.ToLower(strings.TrimSpace(str(app, "current_stage")))
	status := strings.ToLower(strings.TrimSpace(str(app, "current_status")))
	outcome := strings.ToLower(strings.TrimSpace(str(app, "outcome")))
	failure := strings.TrimSpace(str(app, "failure_stage"))
	if stage == "offer" || stage == "结束" || failure != "" {
		return true
	}
	for _, word := range []string{"rejected", "reject", "failed", "closed", "withdrawn", "淘汰", "拒绝", "已挂", "结束", "挂了"} {
		if strings.Contains(status, word) || strings.Contains(outcome, word) {
			return true
		}
	}
	return outcome == "offer" || strings.HasPrefix(outcome, "offer ")
}

func officialURL(app Application) string {
	for _, key := range []string{"official_check_url", "source_url", "jd_url"} {
		if value := strings.TrimSpace(str(app, key)); value != "" {
			return value
		}
	}
	return ""
}

func duplicateGroups(apps []Application) []map[string]any {
	groups := map[string][]Application{}
	for _, app := range apps {
		company := normalize(str(app, "company"))
		role := normalize(str(app, "role"))
		if company == "" || role == "" {
			continue
		}
		key := company + "|" + role
		groups[key] = append(groups[key], app)
	}
	out := []map[string]any{}
	for key, items := range groups {
		if len(items) > 1 {
			out = append(out, map[string]any{"key": key, "count": len(items), "applications": items})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["count"].(int) > out[j]["count"].(int) })
	return out
}

func analytics(state State) map[string]any {
	total, active, interviews, offers := 0, 0, 0, 0
	channels := map[string]int{}
	resumes := map[string]int{}
	failures := map[string]int{}
	funnel := map[string]int{"已投递": 0, "笔试": 0, "一面": 0, "二面": 0, "三面": 0, "HR 面": 0, "Offer": 0}
	trend := map[string]int{}

	resumeNames := map[int]string{}
	for _, rv := range state.ResumeVersions {
		resumeNames[intValue(rv, "id")] = str(rv, "name")
	}

	for _, app := range state.Applications {
		if boolValue(app, "archived") {
			continue
		}
		total++
		if !isTerminal(app) {
			active++
		}
		stage := str(app, "current_stage")
		high := str(app, "highest_stage")
		if stageRank(stage) >= stageRank("一面") || stageRank(high) >= stageRank("一面") {
			interviews++
		}
		if strings.EqualFold(stage, "Offer") || strings.EqualFold(str(app, "outcome"), "Offer") {
			offers++
		}
		if source := str(app, "source"); source != "" {
			channels[source]++
		}
		if rid := intValue(app, "resume_version_id"); rid != 0 {
			name := resumeNames[rid]
			if name == "" {
				name = fmt.Sprintf("简历#%d", rid)
			}
			resumes[name]++
		}
		if failure := str(app, "failure_stage"); failure != "" {
			failures[failure]++
		}
		maxRank := stageRank(high)
		if maxRank < stageRank(stage) {
			maxRank = stageRank(stage)
		}
		for label := range funnel {
			if maxRank >= stageRank(label) {
				funnel[label]++
			}
		}
		if day := str(app, "applied"); len(day) >= 10 {
			trend[day[:10]]++
		}
	}

	return map[string]any{
		"total": total,
		"active": active,
		"interviews": interviews,
		"offers": offers,
		"interview_rate": rate(interviews, total),
		"offer_rate": rate(offers, total),
		"channels": channels,
		"resumes": resumes,
		"failures": failures,
		"funnel": funnel,
		"trend": trend,
	}
}

func rate(n, d int) float64 {
	if d == 0 { return 0 }
	return float64(n) * 100 / float64(d)
}

func stageRank(stage string) int {
	stage = strings.TrimSpace(stage)
	ranks := map[string]int{"待投递": 0, "已投递": 1, "笔试": 2, "一面": 3, "二面": 4, "三面": 5, "HR 面": 6, "Offer": 7, "结束": -1}
	if rank, ok := ranks[stage]; ok { return rank }
	return 1
}

func exportGPT(apps []Application, ids map[int]bool) string {
	var b strings.Builder
	b.WriteString("# 求职数据导出\n\n")	
	selected := 0
	for _, app := range apps {
		id := intValue(app, "id")
		if len(ids) > 0 && !ids[id] { continue }
		selected++
		fmt.Fprintf(&b, "## %s - %s\n\n", str(app, "company"), str(app, "role"))
		fmt.Fprintf(&b, "- ID: %d\n- 地点: %s\n- 渠道: %s\n- 投递日期: %s\n- 当前进度: %s\n- 最高进度: %s\n- 优先级: %s\n- 岗位方向: %s\n- 失败阶段: %s\n- 薪资: %s\n- 官网下次查询: %s\n- 官网查询次数: %d\n\n",
			id, str(app, "city"), str(app, "source"), str(app, "applied"), str(app, "current_stage"), str(app, "highest_stage"), str(app, "priority"), str(app, "job_direction"), str(app, "failure_stage"), str(app, "salary"), str(app, "next_official_check_at"), intValue(app, "official_check_count"))
	}
	fmt.Fprintf(&b, "---\n共 %d 条记录。\n", selected)
	return b.String()
}

func parseIDs(value string) map[int]bool {
	out := map[int]bool{}
	for _, part := range strings.Split(value, ",") {
		if id, err := strconv.Atoi(strings.TrimSpace(part)); err == nil {
			out[id] = true
		}
	}
	return out
}

func normalize(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.Join(strings.Fields(value), "")
	for _, token := range []string{"有限公司", "有限责任公司", "股份有限公司", "（", "）", "(", ")", "-", "_", " "} {
		value = strings.ReplaceAll(value, token, "")
	}
	return value
}

func decodeMap(r *http.Request) (Application, error) {
	m, err := decodeGenericMap(r)
	return Application(m), err
}

func decodeGenericMap(r *http.Request) (map[string]any, error) {
	if !strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		return nil, errors.New("Content-Type must be application/json")
	}
	decoder := json.NewDecoder(io.LimitReader(r.Body, 2<<20))
	decoder.UseNumber()
	var out map[string]any
	if err := decoder.Decode(&out); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	return out, nil
}

func cloneMap[T ~map[string]any](in T) T {
	raw, _ := json.Marshal(in)
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var out T
	_ = decoder.Decode(&out)
	return out
}

func str(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok || v == nil { return "" }
	if s, ok := v.(string); ok { return s }
	return fmt.Sprint(v)
}

func intValue(m map[string]any, key string) int {
	v := m[key]
	switch n := v.(type) {
	case int: return n
	case int64: return int(n)
	case float64: return int(n)
	case json.Number:
		i, _ := n.Int64(); return int(i)
	case string:
		i, _ := strconv.Atoi(n); return i
	default: return 0
	}
}

func boolValue(m map[string]any, key string) bool {
	v := m[key]
	switch b := v.(type) {
	case bool: return b
	case string:
		parsed, _ := strconv.ParseBool(b); return parsed
	default: return false
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}
