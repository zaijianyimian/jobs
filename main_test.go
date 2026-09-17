package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testLocation(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func writeState(t *testing.T, path string, state State) {
	t.Helper()
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationV9SchedulesOfficialCheck(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jobtracker.json")
	writeState(t, path, State{
		SchemaVersion: 9,
		AppVersion:    "9.1",
		Applications: []Application{{
			"id":            1,
			"company":       "测试公司",
			"role":          "Java 后端",
			"source":        "官网",
			"applied":       "2026-09-01",
			"current_stage": "已投递",
		}},
		Events:         []map[string]any{},
		ResumeVersions: []map[string]any{},
	})

	s, err := NewStore(path, testLocation(t))
	if err != nil {
		t.Fatal(err)
	}
	state := s.stateCopy()
	if state.SchemaVersion != 10 {
		t.Fatalf("schema=%d, want 10", state.SchemaVersion)
	}
	app := state.Applications[0]
	if !boolValue(app, "official_check_enabled") {
		t.Fatal("official check should be enabled after migration")
	}
	if got := str(app, "next_official_check_at"); got != "2026-09-08" {
		t.Fatalf("next check=%q, want 2026-09-08", got)
	}
	matches, err := filepath.Glob(path + ".before-v92-*")
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected one migration backup, got %v, err=%v", matches, err)
	}
}

func TestTerminalApplicationStopsCheck(t *testing.T) {
	app := Application{
		"official_check_enabled": true,
		"next_official_check_at": "2020-01-01",
		"current_stage":          "Offer",
	}
	if !isTerminal(app) {
		t.Fatal("Offer should be terminal")
	}
}

func TestRecordOfficialCheckSchedulesSevenDaysLater(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jobtracker.json")
	writeState(t, path, State{
		SchemaVersion: 10,
		AppVersion:    "9.2",
		Applications: []Application{{
			"id":                     1,
			"company":                "测试公司",
			"role":                   "Java",
			"source":                 "官网",
			"current_stage":          "已投递",
			"official_check_enabled": true,
			"next_official_check_at": "2020-01-01",
		}},
		Events:         []map[string]any{},
		ResumeVersions: []map[string]any{},
	})

	loc := testLocation(t)
	s, err := NewStore(path, loc)
	if err != nil {
		t.Fatal(err)
	}
	app, err := s.recordOfficialCheck(1, Application{
		"current_stage": "已投递",
		"note":          "官网仍显示处理中",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := time.Now().In(loc).AddDate(0, 0, 7).Format("2006-01-02")
	if got := str(app, "next_official_check_at"); got != want {
		t.Fatalf("next=%q, want=%q", got, want)
	}
	if got := intValue(app, "official_check_count"); got != 1 {
		t.Fatalf("count=%d, want 1", got)
	}
	if got := str(app, "official_check_note"); got != "官网仍显示处理中" {
		t.Fatalf("note=%q", got)
	}
}

func TestUnknownApplicationFieldsSurviveUpdate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jobtracker.json")
	writeState(t, path, State{
		SchemaVersion: 10,
		AppVersion:    "9.2",
		Applications: []Application{{
			"id":            1,
			"company":       "测试公司",
			"role":          "Java",
			"current_stage": "已投递",
			"future_field":  "must-survive",
		}},
		Events:         []map[string]any{},
		ResumeVersions: []map[string]any{},
	})

	s, err := NewStore(path, testLocation(t))
	if err != nil {
		t.Fatal(err)
	}
	updated, err := s.updateApplication(1, Application{"city": "西安"})
	if err != nil {
		t.Fatal(err)
	}
	if got := str(updated, "future_field"); got != "must-survive" {
		t.Fatalf("unknown field lost: %q", got)
	}
}
