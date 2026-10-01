// Package server wires the hydraulic packages, the persistent store and the
// asynchronous profile runner to net/http. All hydraulic formulas stay in
// their own packages; this package only translates JSON.
package server

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"openchannel/internal/runner"
	"openchannel/internal/store"
)

// dependencies bundles request-scoped singletons. The legacy uniform-flow
// and weir endpoints are pure functions and need none of it, which keeps
// their request/response contract untouched.
type dependencies struct {
	store  *store.Store
	runner *runner.Runner
}

// App bundles the HTTP handler with its lifecycle (the job runner).
type App struct {
	Handler http.Handler
	runner  *runner.Runner
}

// New builds the HTTP handler with default data directory
// OPENCHANNEL_DATA_DIR (or a process-local temp directory when unset) and
// no artificial job step delay.
func New() http.Handler {
	return NewApp(Config{}).Handler
}

// Config customises the server; the zero value means "take defaults".
type Config struct {
	DataDir   string        // persistent data directory
	StepDelay time.Duration // artificial delay per integration step (tests)
}

// NewApp builds the handler with an explicit data directory and starts the
// job runner.
func NewApp(cfg Config) *App {
	dir := cfg.DataDir
	if dir == "" {
		dir = os.Getenv("OPENCHANNEL_DATA_DIR")
	}
	if dir == "" {
		// Container default: the mounted volume. When it is not writable
		// (local `go run` without a mount) fall back to a temp directory
		// rather than failing to start.
		dir = "/data"
		if err := ensureWritable(dir); err != nil {
			tmp, err2 := os.MkdirTemp("", "openchannel-data-")
			if err2 != nil {
				log.Fatalf("no usable data directory: %v (temp: %v)", err, err2)
			}
			log.Printf("data directory /data not writable (%v); using %s", err, tmp)
			dir = tmp
		}
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		log.Fatalf("bad data directory %q: %v", dir, err)
	}

	st, err := store.Open(abs)
	if err != nil {
		log.Fatalf("cannot open data directory %s: %v", abs, err)
	}
	rn := runner.New(st, cfg.StepDelay)
	log.Printf("data directory: %s", abs)

	dep := &dependencies{store: st, runner: rn}
	return &App{Handler: newMux(dep), runner: rn}
}

// ensureWritable verifies the default data directory can be used.
func ensureWritable(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

// Shutdown gracefully stops the background runner (cancel in-flight jobs
// and persist that state).
func (a *App) Shutdown() {
	a.runner.Shutdown()
}

// NewWithConfig builds a standalone handler with an explicit data
// directory; used by tests that manage runner lifecycle themselves.
func NewWithConfig(cfg Config) http.Handler {
	return NewApp(cfg).Handler
}

func newMux(dep *dependencies) http.Handler {
	mux := http.NewServeMux()

	// legacy endpoints — unchanged
	mux.HandleFunc("GET /healthz", health)
	mux.HandleFunc("POST /v1/uniform-flow", uniformFlow)
	mux.HandleFunc("POST /v1/weir-flow", weirFlow)

	// channel lines and versions
	mux.HandleFunc("POST /v1/channels", dep.createChannel)
	mux.HandleFunc("GET /v1/channels", dep.listChannels)
	mux.HandleFunc("GET /v1/channels/{channelID}", dep.getChannel)
	mux.HandleFunc("GET /v1/channels/{channelID}/versions/{version}", dep.getVersion)
	mux.HandleFunc("POST /v1/channels/{channelID}/versions", dep.addVersion)

	// async profile jobs
	mux.HandleFunc("POST /v1/channels/{channelID}/versions/{version}/profile-jobs", dep.submitProfile)
	mux.HandleFunc("GET /v1/jobs/{jobID}", dep.getJob)
	mux.HandleFunc("POST /v1/jobs/{jobID}/cancel", dep.cancelJob)
	mux.HandleFunc("GET /v1/channels/{channelID}/versions/{version}/profile-result", dep.getResult)

	return mux
}
