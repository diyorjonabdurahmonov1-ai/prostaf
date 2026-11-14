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
	"prostaff/internal/blob"
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
	database := db.Open(cfg.DatabaseURL(), cfg.DBMaxOpenConns, cfg.DBMaxIdleConns)
	defer database.Close()

	// Where uploaded photos are written. A directory that cannot be written to
	// is fatal here rather than a 500 on the first upload of the day: on a
	// server the usual cause is a volume that failed to mount, and the deploy
	// should stop instead of quietly accepting photos it cannot keep.
	blobs, err := blob.New(cfg.UploadDir)
	if err != nil {
		log.Fatalf("uploads: %v", err)
	}

	st := store.New(cfg, database, blobs)

	switch {
	case !blobs.Enabled():
		log.Println("uploads: UPLOAD_DIR is not set — images are stored in Postgres " +
			"(fine on an ephemeral host, expensive on a server with a disk)")
	case cfg.UploadMigrate:
		moved, err := st.MigrateFilesToDisk()
		if err != nil {
			log.Fatalf("uploads: migration failed after %d file(s): %v", moved, err)
		}
		if moved > 0 {
			log.Printf("uploads: moved %d image(s) to %s. Postgres does not hand the "+
				"freed pages back on its own — run VACUUM FULL files to reclaim the disk",
				moved, blobs.Dir())
		}
		log.Printf("uploads: writing to %s", blobs.Dir())
	default:
		log.Printf("uploads: writing to %s", blobs.Dir())
		if n := st.CountFilesInDB(); n > 0 {
			log.Printf("uploads: %d older image(s) are still inside the database. "+
				"They are served as before; set UPLOAD_MIGRATE=true to move them to disk", n)
		}
	}
	// Searches whose dispatch loop died with the previous process would otherwise
	// stay open forever — close the ones whose window has already run out.
	if n := st.ExpireStaleSearches(); n > 0 {
		log.Printf("expired %d stale order search(es) left over from the last run", n)
	}

	// A finished job's photos are deleted as it closes; this is the sweeper that
	// covers what that cannot reach — the searches expired in bulk above, an
	// order whose delete hit a full disk, and the whole backlog on the first run
	// after this shipped. See internal/store/order_photos.go.
	switch {
	case !cfg.PurgeOrderPhotos:
		log.Println("order photos: PURGE_ORDER_PHOTOS is off — every photo of every " +
			"finished job is kept, which is the fastest way to fill a small disk")
	default:
		if n := st.CountPurgeableOrders(); n > 0 {
			log.Printf("order photos: %d closed order(s) still carrying photos — "+
				"clearing them in batches, a few sweeps apart", n)
		}
		go watchOrderPhotos(st, cfg.OrderPhotoGraceDays)
	}

	hub := sse.New()
	srv := api.NewServer(cfg, st, hub)
	// Firms pay by the month, so somebody has to notice when a month is nearly
	// over. Nothing else on the platform is time-driven — every other message is
	// a reaction to a request — which is why this needs its own loop.
	go srv.WatchSubscriptions()
	// Said in the log at every start, so a platform running on a default key or
	// password is never a secret the server keeps to itself.
	srv.LogSecurityChecks()

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

// photoSweepInterval is how often the leftovers are collected.
//
// Not a deadline for anything: closing an order deletes its own photos, so this
// only ever finds what that missed. Fifteen minutes is frequent enough that a
// night of expired searches never accumulates and rare enough that the query
// behind it — an index lookup returning nothing, almost every time — costs
// nothing worth measuring.
const photoSweepInterval = 15 * time.Minute

// watchOrderPhotos deletes the photos of orders that are over.
//
// The first pass runs immediately, because the reason to restart a server that
// is short of disk is usually to make it start reclaiming some.
func watchOrderPhotos(st *store.Store, graceDays int) {
	grace := time.Duration(graceDays) * 24 * time.Hour
	if graceDays > 0 {
		log.Printf("order photos: deleted %d day(s) after a job closes", graceDays)
	} else {
		log.Println("order photos: deleted as a job closes")
	}
	st.PurgeClosedOrderPhotos(grace)
	for range time.Tick(photoSweepInterval) {
		st.PurgeClosedOrderPhotos(grace)
	}
}
