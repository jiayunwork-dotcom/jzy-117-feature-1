// Package runner executes asynchronous water-surface-profile jobs.
//
// Execution model: one worker goroutine drains an in-memory queue, so jobs
// for the same channel/version can never run concurrently and two pushes can
// never interleave writes into one result. Submission is idempotent per
// channel version:
//
//   - a succeeded job/result already exists -> reuse it (unless stale),
//   - a queued/running job exists -> return that job,
//   - an interrupted/canceled/failed job or a stale success -> new job.
//
// A restart cannot resume a march mid-step, so on startup the store marks
// any queued/running job as "interrupted" (a terminal state) and a fresh job
// may be submitted. Progress lives partly in memory and is persisted on a
// throttle; cancellation is in-memory via a context.
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"time"

	"openchannel/internal/profile"
	"openchannel/internal/store"
)

// ErrClosed is returned by Submit after the runner has been stopped.
var ErrClosed = errors.New("runner: closed")

// Comparison summarises the depth change against the previous version's
// latest result, sampled on a common xi grid.
type Comparison struct {
	FromVersion      int     `json:"from_version"`
	ToVersion        int     `json:"to_version"`
	MaxDepthIncrease float64 `json:"max_depth_increase_m"`
	MaxDepthDecrease float64 `json:"max_depth_decrease_m"`
	MaxAbsDelta      float64 `json:"max_abs_delta_m"`
	MaxAbsDeltaAt    float64 `json:"max_abs_delta_at_distance_m"`
	MeanAbsDelta     float64 `json:"mean_abs_delta_m"`
	// DownstreamReachUnchanged is reported for multi-reach lines: the
	// downstream reach (unchanged here is the caller's guarantee) matches
	// within tolerance.
	DownstreamReachMaxDelta float64 `json:"downstream_reach_max_delta_m"`
}

// ResultEnvelope is the persisted result document: the profile plus the
// version it belongs to and an optional new-vs-old comparison.
type ResultEnvelope struct {
	ChannelID  string           `json:"channel_id"`
	Version    int              `json:"version"`
	JobID      string           `json:"job_id"`
	Flow       float64          `json:"flow_m3s"`
	Profile    *profile.Profile `json:"profile"`
	Comparison *Comparison      `json:"comparison,omitempty"`
	ComputedAt time.Time        `json:"computed_at"`
}

// Runner owns the queue and worker.
type Runner struct {
	store     *store.Store
	stepDelay time.Duration // artificial delay per step (tests); 0 disables

	queue chan string
	done  chan struct{} // closed once the worker has exited

	mu       sync.Mutex
	cancels  map[string]context.CancelFunc
	submitMu sync.Mutex // serialises the check-and-create on submission
	closed   bool
	hardStop bool // true after Kill: do not persist terminal states
}

// New constructs a runner and recovers jobs left over from a previous run.
func New(st *store.Store, stepDelay time.Duration) *Runner {
	r := &Runner{
		store:     st,
		stepDelay: stepDelay,
		queue:     make(chan string, 1024),
		done:      make(chan struct{}),
		cancels:   map[string]context.CancelFunc{},
	}
	if recovered, err := st.RecoverInterrupted(); err != nil {
		log.Printf("runner: job recovery: %v", err)
	} else if len(recovered) > 0 {
		log.Printf("runner: marked %d unfinished job(s) interrupted after restart", len(recovered))
	}
	go r.worker()
	return r
}

func (r *Runner) isClosed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

// Kill simulates a hard process stop: in-flight work is aborted and the
// worker stops, but job states on disk are left as queued/running, so the
// next process over the same data directory recovers them as
// "interrupted". Kill waits until the worker has actually stopped, which
// guarantees a reopened store cannot race with a dying worker.
func (r *Runner) Kill() {
	r.stop(true)
}

// Shutdown performs a graceful stop: running jobs are canceled and that
// cancellation is persisted before the worker exits.
func (r *Runner) Shutdown() {
	r.stop(false)
}

func (r *Runner) stop(kill bool) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		<-r.done
		return
	}
	r.closed = true
	r.hardStop = kill
	close(r.queue)
	ids := make([]string, 0, len(r.cancels))
	for id, cancel := range r.cancels {
		ids = append(ids, id)
		cancel()
	}
	r.mu.Unlock()
	<-r.done

	if kill {
		return
	}
	// Graceful shutdown: any in-flight job that did not transition before
	// the worker exited is persisted as canceled.
	for _, id := range ids {
		if _, err := r.store.UpdateJob(id, func(j *store.Job) error {
			if j.Status == store.JobQueued || j.Status == store.JobRunning {
				now := time.Now().UTC()
				j.Status = store.JobCanceled
				j.FinishedAt = &now
				if j.Error == "" {
					j.Error = "canceled during shutdown"
				}
			}
			return nil
		}); err != nil {
			log.Printf("runner: shutdown cancel %s: %v", id, err)
		}
	}
}

// progressPersistInterval bounds store writes while a job marches.
const progressPersistInterval = 250 * time.Millisecond

// Submit enqueues a profile job for a channel version with deterministic
// reuse semantics. The returned job is the one to poll (possibly a reused
// one). When reused is true, no new job was created; the reason says why.
func (r *Runner) Submit(channelID string, version int) (job *store.Job, reused bool, reason string, err error) {
	if r.isClosed() {
		return nil, false, "", ErrClosed
	}
	// Serialise check-and-create so two simultaneous submissions for the
	// same channel version cannot both pass the reuse checks.
	r.submitMu.Lock()
	defer r.submitMu.Unlock()
	if active := r.store.ActiveJob(channelID, version); active != nil {
		return active, true, "job already " + active.Status, nil
	}
	if latest, gerr := r.store.LatestSuccess(channelID, version); gerr == nil && !latest.ResultStale {
		return latest, true, "result already computed for this version", nil
	}

	id := newID()
	j := &store.Job{
		ID:        id,
		ChannelID: channelID,
		Version:   version,
		Status:    store.JobQueued,
	}
	if err := r.store.CreateJob(j); err != nil {
		return nil, false, "", err
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, false, "", ErrClosed
	}
	r.queue <- id
	r.mu.Unlock()
	return j, false, "", nil
}

// ErrJobNotActive means cancel was called on a job already in a terminal
// state.
var ErrJobNotActive = errors.New("runner: job is not queued or running")

// Cancel requests cancellation of a queued/running job. A job in a terminal
// state returns ErrJobNotActive together with the unchanged job.
func (r *Runner) Cancel(id string) (*store.Job, error) {
	j, err := r.store.GetJob(id)
	if err != nil {
		return nil, err
	}
	if j.Status != store.JobQueued && j.Status != store.JobRunning {
		return j, ErrJobNotActive
	}
	r.mu.Lock()
	cancel := r.cancels[id]
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	// A still-queued job is canceled synchronously; a running one finishes
	// the cancellation at its next step.
	return r.store.UpdateJob(id, func(j *store.Job) error {
		if j.Status == store.JobQueued {
			now := time.Now().UTC()
			j.Status = store.JobCanceled
			j.FinishedAt = &now
			j.Error = "canceled before execution started"
		}
		return nil
	})
}

func (r *Runner) worker() {
	defer close(r.done)
	for id := range r.queue {
		r.runOne(id)
	}
}

func (r *Runner) runOne(id string) {
	j, err := r.store.GetJob(id)
	if err != nil {
		log.Printf("runner: job %s disappeared: %v", id, err)
		return
	}
	if j.Status != store.JobQueued {
		return // e.g. canceled while queued
	}
	if r.isClosed() {
		return // hard/graceful shutdown: leave state for recovery
	}

	ver, err := r.store.GetVersion(j.ChannelID, j.Version)
	if err != nil {
		r.fail(id, "failed to load channel version: "+err.Error(), nil)
		return
	}
	var spec profile.Spec
	if err := json.Unmarshal(ver.Spec, &spec); err != nil {
		r.fail(id, "stored channel spec is invalid: "+err.Error(), nil)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	r.mu.Lock()
	// The job may have been canceled before it started.
	if cur, _ := r.store.GetJob(id); cur != nil && cur.Status == store.JobCanceled {
		r.mu.Unlock()
		cancel()
		return
	}
	r.cancels[id] = cancel
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.cancels, id)
		r.mu.Unlock()
		cancel()
	}()

	started := time.Now().UTC()
	if _, err := r.store.UpdateJob(id, func(j *store.Job) error {
		j.Status = store.JobRunning
		j.StartedAt = &started
		return nil
	}); err != nil {
		log.Printf("runner: mark running %s: %v", id, err)
		return
	}

	var lastPersist time.Time
	progressSeen := false
	hooks := profile.Hooks{
		Context: ctx,
		Progress: func(xi, total float64) {
			frac := 0.0
			if total > 0 {
				frac = xi / total
			}
			// Persist the very first step immediately so pollers can
			// observe a running job with advancing progress even when the
			// whole push only takes a few steps; throttle afterwards.
			if progressSeen && time.Since(lastPersist) < progressPersistInterval && frac < 1 {
				return
			}
			progressSeen = true
			lastPersist = time.Now()
			if _, err := r.store.UpdateJob(id, func(j *store.Job) error {
				if j.Status == store.JobRunning && ctx.Err() == nil {
					j.Progress = frac
				}
				return nil
			}); err != nil {
				log.Printf("runner: progress %s: %v", id, err)
			}
		},
	}
	if r.stepDelay > 0 {
		hooks.StepDelay = func() { time.Sleep(r.stepDelay) }
	}

	prof, fail, cerr := profile.Compute(spec, profile.Options{}, hooks)
	if r.isClosed() {
		return // shutdown/kill: recovery (or graceful cancel) sets the state
	}
	if cerr == profile.ErrCanceled {
		r.markCanceled(id)
		return
	}
	if cerr != nil {
		r.fail(id, cerr.Error(), nil)
		return
	}
	if fail != nil {
		fb, _ := json.Marshal(fail)
		r.fail(id, fail.Message, fb)
		return
	}

	env := &ResultEnvelope{
		ChannelID:  j.ChannelID,
		Version:    j.Version,
		JobID:      id,
		Flow:       prof.Flow,
		Profile:    prof,
		ComputedAt: time.Now().UTC(),
	}
	env.Comparison = r.comparePrevious(j.ChannelID, j.Version, prof)

	doc, err := json.Marshal(env)
	if err != nil {
		r.fail(id, err.Error(), nil)
		return
	}
	if _, err := r.store.UpdateJob(id, func(j *store.Job) error {
		// Cancellation racing the final write wins: never attach a result
		// to a job the client managed to cancel.
		if ctx.Err() != nil {
			now := time.Now().UTC()
			j.Status = store.JobCanceled
			j.Progress = 0
			j.FinishedAt = &now
			j.Error = "canceled by the client"
			return nil
		}
		now := time.Now().UTC()
		j.Status = store.JobSucceeded
		j.Progress = 1
		j.Result = doc
		j.FinishedAt = &now
		j.Error = ""
		return nil
	}); err != nil {
		log.Printf("runner: finish %s: %v", id, err)
	}
}
func (r *Runner) markCanceled(id string) {
	if _, err := r.store.UpdateJob(id, func(j *store.Job) error {
		now := time.Now().UTC()
		j.Status = store.JobCanceled
		j.FinishedAt = &now
		j.Error = "canceled by the client"
		return nil
	}); err != nil {
		log.Printf("runner: mark canceled %s: %v", id, err)
	}
}

func (r *Runner) fail(id, msg string, failure json.RawMessage) {
	if _, err := r.store.UpdateJob(id, func(j *store.Job) error {
		now := time.Now().UTC()
		j.Status = store.JobFailed
		j.Error = msg
		j.Failure = failure
		j.FinishedAt = &now
		return nil
	}); err != nil {
		log.Printf("runner: mark failed %s: %v", id, err)
	}
}

// comparePrevious builds the new-version vs previous-version summary using
// the previous version's latest successful result. Returns nil when there
// is nothing to compare against.
func (r *Runner) comparePrevious(channelID string, version int, prof *profile.Profile) *Comparison {
	if version <= 1 {
		return nil
	}
	prev, err := r.store.LatestSuccess(channelID, version-1)
	if err != nil || len(prev.Result) == 0 {
		return nil
	}
	var oldEnv ResultEnvelope
	if err := json.Unmarshal(prev.Result, &oldEnv); err != nil || oldEnv.Profile == nil {
		return nil
	}
	return Compare(oldEnv.Profile, prof, version-1, version)
}
