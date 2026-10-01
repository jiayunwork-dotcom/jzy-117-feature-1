// Command openchannel runs the open-channel hydraulic calculation service.
// Scope: section geometry, Manning uniform-flow iteration, rectangular
// sharp-crested weir discharge, persisted canal lines with version history,
// and asynchronous steady gradually-varied-flow water-surface profiles. It
// deliberately has no UI and performs no pressurised-pipe, unsteady-flow or
// catchment computations.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"openchannel/internal/jobs"
	"openchannel/internal/server"
	"openchannel/internal/store"
)

func main() {
	addr := os.Getenv("OPENCHANNEL_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	dataDir := os.Getenv("OPENCHANNEL_DATA")
	if dataDir == "" {
		dataDir = "/data"
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		log.Fatalf("cannot create data directory %q: %v", dataDir, err)
	}

	st, err := store.Open(filepath.Join(dataDir, "openchannel.db"))
	if err != nil {
		log.Fatalf("cannot open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	mgr, err := jobs.NewManager(st, 4, nil)
	if err != nil {
		log.Fatalf("cannot start job manager: %v", err)
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           server.New(&server.Deps{Store: st, Jobs: mgr}),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		log.Printf("openchannel service listening on %s (data dir %s)", addr, dataDir)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Printf("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("http shutdown: %v", err)
	}
	mgr.Close()
}
