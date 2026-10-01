package jobs

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"openchannel/internal/channel"
	"openchannel/internal/geometry"
	"openchannel/internal/profile"
	"openchannel/internal/store"
)

func caseIReaches() []channel.Reach {
	return []channel.Reach{{
		Length:    6000,
		Section:   geometry.Section{BottomWidth: 3},
		Roughness: 0.015,
		Slope:     0.001,
	}}
}

func weirControl() channel.Control {
	return channel.Control{
		Kind: channel.ControlWeir, Width: 3, CrestHeight: 1.0, Cd: 0.62,
	}
}

type testEnv struct {
	st    *store.Store
	mgr   *Manager
	mu    sync.Mutex
	steps []float64
}

func newEnv(t *testing.T, hook func(jobID string, done, total float64)) *testEnv {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	env := &testEnv{st: st}
	stepHook := StepHook(nil)
	if hook != nil {
		stepHook = func(id string, done, total float64, _ <-chan struct{}) {
			env.mu.Lock()
			env.steps = append(env.steps, done)
			env.mu.Unlock()
			hook(id, done, total)
		}
	}
	mgr, err := NewManager(st, 1, stepHook)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Close)
	env.mgr = mgr
	return env
}

func (e *testEnv) stepCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.steps)
}

func waitTerminal(t *testing.T, st *store.Store, jobID string) *store.Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		j, err := st.GetJob(jobID)
		if err != nil {
			t.Fatal(err)
		}
		switch j.Status {
		case store.StatusSucceeded, store.StatusFailed, store.StatusCancelled, store.StatusInterrupted:
			return j
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("job %s did not reach a terminal state", jobID)
	return nil
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("timed out waiting for: " + msg)
}

// TestSubmitSucceedsAndProducesResult covers the happy async path: submit
// returns an id immediately, status advances, a result lands on the version.
func TestSubmitSucceedsAndProducesResult(t *testing.T) {
	env := newEnv(t, nil)
	def := channel.Definition{Reaches: caseIReaches(), DesignFlow: 5, Control: weirControl()}
	if err := env.st.CreateChannel("c1", def); err != nil {
		t.Fatal(err)
	}
	id, reused, err := env.mgr.Submit(context.Background(), "c1", 1, profile.ModeFull, false)
	if err != nil || id == "" || reused {
		t.Fatalf("Submit: id=%q reused=%v err=%v", id, reused, err)
	}
	j := waitTerminal(t, env.st, id)
	if j.Status != store.StatusSucceeded || j.Profile == nil || j.Profile.Profile == nil {
		t.Fatalf("job = %+v", j)
	}
	if j.Profile.Recompute != "full" {
		t.Errorf("recompute mode = %q, want full", j.Profile.Recompute)
	}
	if len(j.Profile.Profile.Points) == 0 {
		t.Error("profile has no points")
	}
	if !j.Profile.Profile.UpstreamNormalMatch {
		t.Error("upstream end should be at normal depth")
	}
}

// TestProgressAndCancel is case VIII: progress must be observed advancing for
// real, and a cancel issued mid-flight must stop the run without producing a
// result.
func TestProgressAndCancel(t *testing.T) {
	block := make(chan struct{})
	blocked := make(chan struct{})
	var once sync.Once

	env := newEnv(t, func(_ string, done, _ float64) {
		// Park the worker ~1.5 km into the 6 km line, well before the end.
		if done >= 1500 {
			once.Do(func() { close(blocked) })
			<-block
		}
	})
	def := channel.Definition{Reaches: caseIReaches(), DesignFlow: 5, Control: weirControl()}
	if err := env.st.CreateChannel("c1", def); err != nil {
		t.Fatal(err)
	}
	id, _, err := env.mgr.Submit(context.Background(), "c1", 1, profile.ModeFull, false)
	if err != nil {
		t.Fatal(err)
	}

	// Wait until the worker is parked mid-computation.
	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("worker never reached the blocking point")
	}

	// Real progress must have advanced through at least several steps.
	waitFor(t, func() bool { return env.stepCount() >= 5 }, "at least 5 progress steps")
	prog := func() float64 {
		j, _ := env.st.GetJob(id)
		return j.Progress
	}
	waitFor(t, func() bool { return prog() > 0 && prog() < 1 }, "progress strictly between 0 and 1")
	progressAtCancel := prog()
	if !(progressAtCancel > 0.1 && progressAtCancel < 0.5) {
		t.Fatalf("progress at cancel = %.3f, expected inside (0.1, 0.5)", progressAtCancel)
	}

	// Cancel, then let the parked step finish.
	ok, err := env.mgr.Cancel(id)
	if err != nil || !ok {
		t.Fatalf("Cancel = %v, %v", ok, err)
	}
	stepsAtCancel := env.stepCount()
	close(block)

	j := waitTerminal(t, env.st, id)
	if j.Status != store.StatusCancelled {
		t.Fatalf("status = %q, want cancelled", j.Status)
	}
	if j.Profile != nil {
		t.Fatalf("cancelled job produced a result: %+v", j.Profile)
	}

	// Give the engine time to prove it really stopped.
	time.Sleep(50 * time.Millisecond)
	if got := env.stepCount(); got > stepsAtCancel+1 {
		t.Errorf("worker kept integrating after cancel: steps %d -> %d", stepsAtCancel, got)
	}
	j2, _ := env.st.GetJob(id)
	if j2.Status != store.StatusCancelled || j2.Profile != nil {
		t.Fatalf("cancelled state did not stick: %+v", j2)
	}

	// Cancelling a terminal job reports "no transition".
	if ok, _ := env.mgr.Cancel(id); ok {
		t.Error("second Cancel should report false")
	}
}

// TestDuplicateSubmissionReusesJob defines repeat-submit behaviour: while a
// job is active, resubmitting returns the same job; after success the finished
// job is reused too; force starts a fresh one.
func TestDuplicateSubmissionReusesJob(t *testing.T) {
	block := make(chan struct{})
	blocked := make(chan struct{})
	var once sync.Once
	env := newEnv(t, func(_ string, done, _ float64) {
		if done >= 500 {
			once.Do(func() { close(blocked) })
			<-block
		}
	})
	def := channel.Definition{Reaches: caseIReaches(), DesignFlow: 5, Control: weirControl()}
	if err := env.st.CreateChannel("c1", def); err != nil {
		t.Fatal(err)
	}

	id1, _, err := env.mgr.Submit(context.Background(), "c1", 1, profile.ModeFull, false)
	if err != nil {
		t.Fatal(err)
	}
	<-blocked
	id2, reused2, err := env.mgr.Submit(context.Background(), "c1", 1, profile.ModeFull, false)
	if err != nil {
		t.Fatal(err)
	}
	if id2 != id1 || !reused2 {
		t.Fatalf("active resubmit: id=%q want %q, reused=%v", id2, id1, reused2)
	}
	close(block)
	j := waitTerminal(t, env.st, id1)
	if j.Status != store.StatusSucceeded {
		t.Fatalf("status = %q", j.Status)
	}

	// Completed result is reused.
	id3, reused3, _ := env.mgr.Submit(context.Background(), "c1", 1, profile.ModeFull, false)
	if id3 != id1 || !reused3 {
		t.Fatalf("post-success resubmit: id=%q want %q reused=%v", id3, id1, reused3)
	}

	// Force starts a genuinely new job.
	id4, reused4, _ := env.mgr.Submit(context.Background(), "c1", 1, profile.ModeFull, true)
	if id4 == id1 || reused4 {
		t.Fatalf("force resubmit: id=%q reused=%v, want a new job", id4, reused4)
	}
	waitTerminal(t, env.st, id4)
}

// TestSteepFailureThroughManager propagates the hydraulic failure, with
// location, into the job state.
func TestSteepFailureThroughManager(t *testing.T) {
	env := newEnv(t, nil)
	def := channel.Definition{
		Reaches: []channel.Reach{
			{Length: 2000, Section: geometry.Section{BottomWidth: 3}, Roughness: 0.015, Slope: 0.05},
			{Length: 2000, Section: geometry.Section{BottomWidth: 3}, Roughness: 0.015, Slope: 0.001},
		},
		DesignFlow: 5,
		Control:    channel.Control{Kind: channel.ControlDepth, Depth: 1.5},
	}
	if err := env.st.CreateChannel("c1", def); err != nil {
		t.Fatal(err)
	}
	id, _, _ := env.mgr.Submit(context.Background(), "c1", 1, profile.ModeFull, false)
	j := waitTerminal(t, env.st, id)
	if j.Status != store.StatusFailed || j.Failure == nil {
		t.Fatalf("job = %+v", j)
	}
	if j.Failure.Kind != profile.FailSteepReach || j.Failure.Reach != 0 {
		t.Fatalf("failure = %+v", j.Failure)
	}
	if j.Profile != nil {
		t.Error("failed job must not carry a profile")
	}
}

// TestRecoveryOnManagerRestart: a job left active by a previous process is
// marked interrupted when a new manager opens the store, and the version can
// be resubmitted.
func TestRecoveryOnManagerRestart(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	def := channel.Definition{Reaches: caseIReaches(), DesignFlow: 5, Control: weirControl()}
	if err := st.CreateChannel("c1", def); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob("ghost", "c1", 1, "full"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.MarkRunning("ghost"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// New process opens the same data directory.
	st2, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st2.Close() })
	mgr2, err := NewManager(st2, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr2.Close)

	j, err := st2.GetJob("ghost")
	if err != nil || j.Status != store.StatusInterrupted {
		t.Fatalf("ghost job = %+v, %v", j, err)
	}

	// Resubmission is accepted and runs to success.
	id, reused, err := mgr2.Submit(context.Background(), "c1", 1, profile.ModeFull, false)
	if err != nil || reused {
		t.Fatalf("resubmit: id=%q reused=%v err=%v", id, reused, err)
	}
	if id == "ghost" {
		t.Error("resubmission must create a new job, not revive the interrupted one")
	}
	waitTerminal(t, st2, id)
}

// TestPartialRecomputeEndToEnd is case V at the manager level: after changing
// only the upstream reach roughness, the partial run reuses the suffix and
// matches a full force-rerun within tolerance; the comparison names the
// biggest change.
func TestPartialRecomputeEndToEnd(t *testing.T) {
	env := newEnv(t, nil)
	mk := func(n float64) channel.Definition {
		return channel.Definition{
			Reaches: []channel.Reach{
				{Length: 2000, Section: geometry.Section{BottomWidth: 3}, Roughness: n, Slope: 0.001},
				{Length: 3000, Section: geometry.Section{BottomWidth: 3}, Roughness: 0.015, Slope: 0.001},
			},
			DesignFlow: 5,
			Control:    weirControl(),
		}
	}
	if err := env.st.CreateChannel("c1", mk(0.015)); err != nil {
		t.Fatal(err)
	}
	id1, _, _ := env.mgr.Submit(context.Background(), "c1", 1, profile.ModeFull, false)
	j1 := waitTerminal(t, env.st, id1)
	base := j1.Profile.Profile

	if v, err := env.st.UpdateChannel("c1", 1, mk(0.020)); err != nil || v != 2 {
		t.Fatalf("update: v=%d err=%v", v, err)
	}

	// Partial run.
	idP, _, _ := env.mgr.Submit(context.Background(), "c1", 2, profile.ModePartial, false)
	jP := waitTerminal(t, env.st, idP)
	if jP.Status != store.StatusSucceeded {
		t.Fatalf("partial job %+v", jP)
	}
	if jP.Profile.Recompute != "partial" {
		t.Fatalf("recompute = %q, want partial", jP.Profile.Recompute)
	}
	part := jP.Profile.Profile

	// The unchanged downstream reach (s < 2000) is bit-identical.
	for _, bp := range base.Points {
		if bp.Distance < 2000 {
			for _, pp := range part.Points {
				if pp.Distance == bp.Distance && pp.Depth != bp.Depth {
					t.Fatalf("suffix point %.0f changed: %.12f != %.12f",
						pp.Distance, pp.Depth, bp.Depth)
				}
			}
		}
	}

	// Comparison summary exists and locates the biggest change upstream.
	comp := jP.Profile.Comparison
	if comp == nil || comp.MaxDeltaDistance < 2000 {
		t.Fatalf("comparison = %+v", comp)
	}
	if comp.MaxDeltaDepth <= 0 {
		t.Errorf("rougher upstream reach should raise depth, delta = %.2e", comp.MaxDeltaDepth)
	}

	// Full force-rerun of the same version must agree with the partial run.
	idF, _, _ := env.mgr.Submit(context.Background(), "c1", 2, profile.ModeFull, true)
	jF := waitTerminal(t, env.st, idF)
	full := jF.Profile.Profile
	var maxErr float64
	for _, fp := range full.Points {
		for _, pp := range part.Points {
			if pp.Distance == fp.Distance {
				if e := absF(pp.Depth - fp.Depth); e > maxErr {
					maxErr = e
				}
			}
		}
	}
	if maxErr > 1e-9 {
		t.Errorf("partial vs full max depth discrepancy %.2e > 1e-9", maxErr)
	}
}

// TestSubmitUnknown rejects references to missing channels/versions.
func TestSubmitUnknown(t *testing.T) {
	env := newEnv(t, nil)
	if _, _, err := env.mgr.Submit(context.Background(), "ghost", 1, profile.ModeFull, false); err != store.ErrNotFound {
		t.Fatalf("missing channel: %v", err)
	}
	def := channel.Definition{Reaches: caseIReaches(), DesignFlow: 5, Control: weirControl()}
	if err := env.st.CreateChannel("c1", def); err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.mgr.Submit(context.Background(), "c1", 9, profile.ModeFull, false); err != store.ErrNotFound {
		t.Fatalf("missing version: %v", err)
	}
}

func absF(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
