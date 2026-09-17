package main

import (
	"log"
	"net/http"
	"os"
	"strings"
	"time"
	_ "time/tzdata"
)

const (
	schemaVersion = 10
	appVersion    = "9.2"
)

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
