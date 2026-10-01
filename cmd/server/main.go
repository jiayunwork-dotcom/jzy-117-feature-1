// Command openchannel runs the open-channel hydraulic calculation service.
// Scope: section geometry, Manning uniform-flow iteration, rectangular
// sharp-crested weir discharge, persistent channel-line versions and
// asynchronous steady gradually-varied-flow profile jobs. It deliberately
// has no UI and performs no unsteady-flow or pressurised-pipe computations.
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

	"openchannel/internal/server"
)

func main() {
	addr := os.Getenv("OPENCHANNEL_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	app := server.NewApp(server.Config{})

	srv := &http.Server{
		Addr:              addr,
		Handler:           app.Handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		log.Printf("openchannel service listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	<-stop
	log.Printf("shutdown signal received")

	// Stop taking new HTTP connections; in-flight requests get a grace
	// period, then the job runner cancels running profile jobs and
	// persists that terminal state.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("http shutdown: %v", err)
	}
	app.Shutdown()
	log.Printf("shutdown complete")
}
