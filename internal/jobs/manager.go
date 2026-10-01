// Package jobs runs asynchronous GVF computations over stored canal versions.
//
// Lifecycle rules
//
//   - Submitting a computation for a channel/version that already has a
//     pending or running job reuses that job (its id is returned); a finished
//     result is reused as well, unless force=true. There is therefore never
//     more than one writer for a version's result, enforced in the store.
//   - Cancellation is cooperative: the engine checks the cancel channel once
//     per spatial step, and the terminal store transition (cancelled) wins
//     over a late completion because updates are guarded by the active state.
//   - Profiles are written into the job row together with the succeeded
//     status in one statement, so a reader can never see a half-written
//     water-surface profile.
//   - On restart the store marks all jobs that were still active as
//     "interrupted"; they are never stuck in progress and may be resubmitted.
//     (Resuming silently was rejected: a computation is seconds at most, and
//     an explicit terminal state is easier to reason about than resurrecting
//     workers whose process died.)
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"sync"
	"time"

	"openchannel/internal/channel"
	"openchannel/internal/profile"
	"openchannel/internal/store"
)

// StepHook is a test-only synchronization point invoked after each integrated
// step. It receives the same cancel/progress plumbing the engine uses.
type StepHook func(jobID string, doneMetres, totalMetres float64, cancel <-chan struct{})

// ErrInvalidMode is returned when a submission asks for an unknown recompute
// mode.
var ErrInvalidMode = errors.New(`jobs: mode must be "full" or "partial"`)

// Manager owns the worker pool and the per-job cancellation handles.
type Manager struct {
	st   *store.Store
	sem  chan struct{}
	wg   sync.WaitGroup
	mu   sync.Mutex
	canc map[string]chan struct{}
	hook StepHook
}

// NewManager creates a manager with up to maxConcurrent running profiles and
// performs startup recovery (active jobs from a previous process are marked
// interrupted).
func NewManager(st *store.Store, maxConcurrent int, hook StepHook) (*Manager, error) {
	if maxConcurrent < 1 {
		maxConcurrent = 4
	}
	if n, err := st.RecoverInterrupted(); err != nil {
		return nil, err
	} else if n > 0 {
		log.Printf("jobs: marked %d in-flight job(s) from a previous process as interrupted", n)
	}
	return &Manager{
		st:   st,
		sem:  make(chan struct{}, maxConcurrent),
		canc: make(map[string]chan struct{}),
		hook: hook,
	}, nil
}

// Close waits for in-process workers to finish. Call it on graceful shutdown.
func (m *Manager) Close() { m.wg.Wait() }

// Submit requests a profile computation.
//
// mode is "full" or "partial" (other values are rejected). With "partial" the
// manager decides from the previous version's definition whether partial
// recomputation is actually applicable and transparently falls back to full
// when it is not; the mode actually used is recorded on the job/result.
//
// force allows replacing a terminal result for the version by creating a new
// job; it must be false for plain reuse semantics.
func (m *Manager) Submit(ctx context.Context, channelID string, version int, mode string, force bool) (jobID string, reused bool, err error) {
	if mode != profile.ModeFull && mode != profile.ModePartial {
		return "", false, ErrInvalidMode
	}
	if exists, err := m.st.ChannelExists(channelID); err != nil {
		return "", false, err
	} else if !exists {
		return "", false, store.ErrNotFound
	}
	if _, err := m.st.GetDefinition(channelID, version); err != nil {
		return "", false, err
	}

	if !force {
		if active, err := m.st.ActiveJob(channelID, version); err == nil {
			return active.ID, true, nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return "", false, err
		}
		if latest, err := m.st.LatestJobForVersion(channelID, version); err == nil {
			if latest.Status == store.StatusSucceeded {
				return latest.ID, true, nil
			}
		} else if !errors.Is(err, store.ErrNotFound) {
			return "", false, err
		}
	}

	id, err := newID()
	if err != nil {
		return "", false, err
	}
	if err := m.st.CreateJob(id, channelID, version, mode); err != nil {
		if errors.Is(err, store.ErrActiveJob) {
			active, gErr := m.st.ActiveJob(channelID, version)
			if gErr != nil {
				return "", false, gErr
			}
			return active.ID, true, nil
		}
		return "", false, err
	}

	// Register the cancel handle before the worker exists, so a cancel that
	// arrives while the job is queued still unblocks the worker.
	stop := make(chan struct{})
	m.mu.Lock()
	m.canc[id] = stop
	m.mu.Unlock()
	m.start(id, stop)
	return id, false, nil
}

func (m *Manager) start(id string, stop chan struct{}) {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer func() {
			m.mu.Lock()
			delete(m.canc, id)
			m.mu.Unlock()
		}()
		m.run(id, stop)
	}()
}

// Cancel requests cancellation. Only an active job can be cancelled: an
// unknown job id returns store.ErrNotFound, an already-terminal job returns
// (false, nil).
func (m *Manager) Cancel(id string) (bool, error) {
	if _, err := m.st.GetJob(id); err != nil {
		return false, err
	}
	ok, err := m.st.Cancel(id)
	if err != nil {
		return false, err
	}
	if ok {
		m.mu.Lock()
		ch := m.canc[id]
		m.mu.Unlock()
		if ch != nil {
			close(ch)
		}
	}
	return ok, nil
}

func (m *Manager) run(id string, stop chan struct{}) {
	// Acquire a worker slot. Cancellation while queued must not deadlock.
	select {
	case m.sem <- struct{}{}:
	case <-stop:
		return
	}
	defer func() { <-m.sem }()

	running, err := m.st.MarkRunning(id)
	if err != nil || !running {
		return // cancelled/terminal while queued, or store error
	}

	job, err := m.st.GetJob(id)
	if err != nil {
		_ = m.st.FinishFailed(id, nil, err.Error())
		return
	}
	def, err := m.st.GetDefinition(job.ChannelID, job.Version)
	if err != nil {
		_ = m.st.FinishFailed(id, nil, err.Error())
		return
	}

	prof, modeUsed, comp, failErr := m.compute(job, def, stop)
	if failErr != nil {
		if errors.Is(failErr, profile.ErrCancelled) {
			// Store transition is the source of truth; if MarkRunning's
			// guard raced and the job already moved to cancelled, Finish* is
			// simply a no-op here (we do not overwrite a terminal state).
			if _, err := m.st.Cancel(id); err != nil {
				log.Printf("jobs: cancel finalise for %s: %v", id, err)
			}
			return
		}
		var cf *profile.ComputeFailure
		if errors.As(failErr, &cf) {
			_ = m.st.FinishFailed(id, cf, cf.Error())
		} else {
			_ = m.st.FinishFailed(id, nil, failErr.Error())
		}
		return
	}

	doc := &store.ProfileDocument{
		Profile:    prof,
		Recompute:  modeUsed,
		Comparison: comp,
		JobID:      id,
		ComputedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := m.st.FinishSucceeded(id, doc); err != nil {
		log.Printf("jobs: finishing %s: %v", id, err)
	}
}

// compute selects full vs partial mode, loads the base profile for comparison
// and runs the engine.
func (m *Manager) compute(job *store.Job, def channel.Definition, stop chan struct{}) (
	*profile.Profile, string, *profile.Comparison, error) {

	hook := profile.Hooks{
		Cancel: stop,
		Progress: func(done, total float64) {
			frac := 0.0
			if total > 0 {
				frac = done / total
			}
			_ = m.st.UpdateProgress(job.ID, frac)
			if m.hook != nil {
				m.hook(job.ID, done, total, stop)
			}
		},
	}

	// Load the directly preceding version + its latest succeeded profile, for
	// both partial reuse and the comparison summary.
	baseVersion := job.Version - 1
	var baseDef channel.Definition
	var baseProf *profile.Profile
	if baseVersion >= 1 {
		if d, err := m.st.GetDefinition(job.ChannelID, baseVersion); err == nil {
			baseDef = d
		}
		if bj, err := m.st.LatestJobForVersion(job.ChannelID, baseVersion); err == nil &&
			bj.Status == store.StatusSucceeded && bj.Profile != nil {
			baseProf = bj.Profile.Profile
		}
	}

	if job.Recompute == profile.ModePartial && baseProf != nil {
		if first, ok := profile.PartialEligible(baseDef, def); ok {
			p, err := profile.RecomputePartial(baseDef, def, first, baseProf, hook)
			if err != nil {
				return nil, "", nil, err
			}
			comp := profile.CompareProfiles(baseVersion, baseProf, p)
			return p, profile.ModePartial, comp, nil
		}
		// Not eligible: fall through to full recompute.
	}

	p, err := profile.Compute(def, hook)
	if err != nil {
		return nil, "", nil, err
	}
	var comp *profile.Comparison
	if baseProf != nil {
		comp = profile.CompareProfiles(baseVersion, baseProf, p)
	}
	return p, profile.ModeFull, comp, nil
}

// newID returns a 128-bit random hex identifier.
func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
