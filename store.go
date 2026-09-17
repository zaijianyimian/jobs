package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Application map[string]any

type State struct {
	SchemaVersion  int             `json:"schema_version"`
	AppVersion     string          `json:"app_version"`
	Applications   []Application   `json:"applications"`
	Events         []map[string]any `json:"events"`
	ResumeVersions []map[string]any `json:"resume_versions"`
}

type Store struct {
	mu   sync.RWMutex
	path string
	loc  *time.Location
	data State
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
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
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

func intValue(m map[string]any, key string) int {
	v := m[key]
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	case string:
		i, _ := strconv.Atoi(n)
		return i
	default:
		return 0
	}
}
