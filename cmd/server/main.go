// Command openchannel runs the open-channel hydraulic calculation service.
// Scope: section geometry, Manning uniform-flow iteration and rectangular
// sharp-crested weir discharge. It deliberately has no UI and performs no
// pressurised-pipe or catchment computations.
package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"openchannel/internal/server"
)

func main() {
	addr := os.Getenv("OPENCHANNEL_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           server.New(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	log.Printf("openchannel service listening on %s", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
