// Command server starts the PROSTAFF HTTP API.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"prostaff/internal/api"
	"prostaff/internal/config"
	"prostaff/internal/db"
	"prostaff/internal/sse"
	"prostaff/internal/store"
)

func main() {
	// Load .env file if it exists (ignored in production).
	if err := godotenv.Load(); err != nil {
		log.Println("no .env file found, using system environment")
	}

	cfg := config.Load()

	// Open (or create) PostgreSQL database.
	database := db.Open(cfg.DatabaseURL())
	defer database.Close()

	st := store.New(cfg, database)
	// Searches whose dispatch loop died with the previous process would otherwise
	// stay open forever — close the ones whose window has already run out.
	if n := st.ExpireStaleSearches(); n > 0 {
		log.Printf("expired %d stale order search(es) left over from the last run", n)
	}
	hub := sse.New()
	srv := api.NewServer(cfg, st, hub)
	// Firms pay by the month, so somebody has to notice when a month is nearly
	// over. Nothing else on the platform is time-driven — every other message is
	// a reaction to a request — which is why this needs its own loop.
	go srv.WatchSubscriptions()

	httpServer := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: SSE connections are long-lived.
	}

	// Start the server.
	go func() {
		log.Printf("PROSTAFF backend listening on :%s (dev_mode=%v, commission=%d%%)",
			cfg.Port, cfg.DevMode, cfg.CommissionPercent)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server error: %v", err)
		}
	}()

	// Wait for an interrupt/terminate signal.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Println("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		log.Printf("graceful shutdown failed: %v", err)
	}
	log.Println("bye")
}
