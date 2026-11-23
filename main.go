// Command exercise-logger is a tiny web app for logging workout sessions.
//
// It is intentionally dependency-light: the standard library's net/http and
// html/template do all the web work, and SQLite (via the pure-Go
// modernc.org/sqlite driver, so no CGO is needed) stores the data in a single
// file. That makes it trivial to cross-compile and run on a Raspberry Pi.
package main

import (
	"context"
	"embed"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	// Embeds the IANA timezone database into the binary so the TZ
	// environment variable works even in a "FROM scratch" container.
	_ "time/tzdata"
)

// assets holds the HTML templates and the stylesheet, compiled into the
// binary so the app ships as one self-contained file.
//
//go:embed templates static
var assets embed.FS

func main() {
	addr := getenv("ADDR", ":8080")
	dbPath := getenv("DB_PATH", "data/logger.db")

	store, err := OpenStore(dbPath)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer store.Close()

	app, err := NewApp(store, getenv("WEIGHT_UNIT", "kg"))
	if err != nil {
		log.Fatalf("init app: %v", err)
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           logRequests(app.Routes()),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Shut down cleanly on Ctrl+C or `docker stop` (SIGTERM) so SQLite can
	// flush its write-ahead log before the process exits.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("listening on %s (db: %s)", addr, dbPath)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// logRequests is a minimal middleware that logs method, path and duration.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Microsecond))
	})
}
