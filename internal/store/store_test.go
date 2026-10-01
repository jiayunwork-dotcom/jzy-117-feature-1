package store

import (
	"errors"
	"path/filepath"
	"testing"

	"openchannel/internal/channel"
	"openchannel/internal/geometry"
	"openchannel/internal/profile"
)

func sampleDef(crest float64) channel.Definition {
	return channel.Definition{
		Reaches: []channel.Reach{{
			Length:    1000,
			Section:   geometry.Section{BottomWidth: 3},
			Roughness: 0.015,
			Slope:     0.001,
		}},
		DesignFlow: 5,
		Control: channel.Control{
			Kind:        channel.ControlWeir,
			Width:       3,
			CrestHeight: crest,
			Cd:          0.62,
		},
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestCreateGetAndVersionHistory(t *testing.T) {
	st := openTestStore(t)
	def := sampleDef(1.0)
	if err := st.CreateChannel("c1", def); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetDefinition("c1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Control.CrestHeight != 1.0 || len(got.Reaches) != 1 {
		t.Fatalf("definition round-trip mismatch: %+v", got)
	}
	if v, err := st.LatestVersion("c1"); err != nil || v != 1 {
		t.Fatalf("latest = %d, %v", v, err)
	}

	v2, err := st.UpdateChannel("c1", 1, sampleDef(1.2))
	if err != nil {
		t.Fatal(err)
	}
	if v2 != 2 {
		t.Fatalf("new version = %d, want 2", v2)
	}
	infos, err := st.ListVersions("c1")
	if err != nil || len(infos) != 2 || infos[0].Version != 1 || infos[1].Version != 2 {
		t.Fatalf("history = %+v, err %v", infos, err)
	}
	// Old version is still readable, unchanged.
	old, _ := st.GetDefinition("c1", 1)
	if old.Control.CrestHeight != 1.0 {
		t.Errorf("old version mutated: crest = %v", old.Control.CrestHeight)
	}
}

// TestOptimisticConcurrency is case VII's conflict rule at the store level.
func TestOptimisticConcurrency(t *testing.T) {
	st := openTestStore(t)
	if err := st.CreateChannel("c1", sampleDef(1.0)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateChannel("c1", 1, sampleDef(1.1)); err != nil {
		t.Fatal(err)
	}
	// Someone submits a change based on the now-stale version 1.
	_, err := st.UpdateChannel("c1", 1, sampleDef(1.2))
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("err = %v, want ErrVersionConflict", err)
	}
	// The rejected commit changed nothing.
	if v, _ := st.LatestVersion("c1"); v != 2 {
		t.Errorf("latest = %d, want 2 (conflict must not write)", v)
	}
	// Basing on the current version succeeds.
	if _, err := st.UpdateChannel("c1", 2, sampleDef(1.2)); err != nil {
		t.Fatalf("update from current version: %v", err)
	}
}

func TestUnknownChannel(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.LatestVersion("ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("latest ghost: %v", err)
	}
	if _, err := st.GetDefinition("ghost", 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("get ghost: %v", err)
	}
	if _, err := st.UpdateChannel("ghost", 1, sampleDef(1)); !errors.Is(err, ErrNotFound) {
		t.Errorf("update ghost: %v", err)
	}
	if _, err := st.ListVersions("ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("history ghost: %v", err)
	}
}

func TestJobLifecycleAndResultAtomicallyPresent(t *testing.T) {
	st := openTestStore(t)
	if err := st.CreateChannel("c1", sampleDef(1.0)); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob("j1", "c1", 1, "full"); err != nil {
		t.Fatal(err)
	}
	// Second active job for the same version is rejected.
	if err := st.CreateJob("j2", "c1", 1, "full"); !errors.Is(err, ErrActiveJob) {
		t.Fatalf("second active job: %v", err)
	}
	active, err := st.ActiveJob("c1", 1)
	if err != nil || active.ID != "j1" {
		t.Fatalf("active job = %+v, %v", active, err)
	}
	if ok, err := st.MarkRunning("j1"); !ok || err != nil {
		t.Fatalf("MarkRunning = %v, %v", ok, err)
	}
	if err := st.UpdateProgress("j1", 0.5); err != nil {
		t.Fatal(err)
	}
	doc := &ProfileDocument{
		Profile:   &profile.Profile{TotalLength: 1000, DownstreamDepth: 1.9},
		Recompute: "full",
		JobID:     "j1",
	}
	if err := st.FinishSucceeded("j1", doc); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetJob("j1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusSucceeded || got.Profile == nil || got.Profile.Profile.TotalLength != 1000 {
		t.Fatalf("terminal job inconsistent: %+v", got)
	}
	// After the terminal transition another job for the version is allowed.
	if err := st.CreateJob("j3", "c1", 1, "full"); err != nil {
		t.Fatalf("new job after completion: %v", err)
	}
}

func TestFailurePersistsLocation(t *testing.T) {
	st := openTestStore(t)
	_ = st.CreateChannel("c1", sampleDef(1.0))
	_ = st.CreateJob("j1", "c1", 1, "full")
	cf := &profile.ComputeFailure{Kind: profile.FailSteepReach, Reach: 2, Distance: 3200, Message: "boom"}
	if err := st.FinishFailed("j1", cf, cf.Error()); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetJob("j1")
	if got.Status != StatusFailed || got.Failure == nil ||
		got.Failure.Reach != 2 || got.Failure.Distance != 3200 {
		t.Fatalf("failure not persisted: %+v", got)
	}
	if got.Profile != nil {
		t.Error("failed job must carry no profile")
	}
}

// TestRecoveryAcrossReopen covers case VII's restart rules: channel, history
// and finished results survive reopening the data directory, while a job that
// was still running is marked interrupted instead of hanging.
func TestRecoveryAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateChannel("c1", sampleDef(1.0)); err != nil {
		t.Fatal(err)
	}
	_, _ = st.UpdateChannel("c1", 1, sampleDef(1.1))

	// Finished job with result.
	if err := st.CreateJob("done", "c1", 1, "full"); err != nil {
		t.Fatal(err)
	}
	_, _ = st.MarkRunning("done")
	if err := st.FinishSucceeded("done", &ProfileDocument{
		Profile: &profile.Profile{TotalLength: 1000}, Recompute: "full", JobID: "done",
	}); err != nil {
		t.Fatal(err)
	}

	// Job left running when the process dies.
	if err := st.CreateJob("stuck", "c1", 2, "full"); err != nil {
		t.Fatal(err)
	}
	_, _ = st.MarkRunning("stuck")
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st2.Close() }()

	n, err := st2.RecoverInterrupted()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("recovered %d jobs, want 1", n)
	}
	stuck, err := st2.GetJob("stuck")
	if err != nil {
		t.Fatal(err)
	}
	if stuck.Status != StatusInterrupted {
		t.Errorf("stuck job status = %q, want interrupted", stuck.Status)
	}
	// Recovery is idempotent on the next boot.
	if n, _ := st2.RecoverInterrupted(); n != 0 {
		t.Errorf("second recovery touched %d jobs, want 0", n)
	}

	// Channel, history and finished result are all still there.
	if v, _ := st2.LatestVersion("c1"); v != 2 {
		t.Errorf("latest after reopen = %d, want 2", v)
	}
	def, err := st2.GetDefinition("c1", 2)
	if err != nil || def.Control.CrestHeight != 1.1 {
		t.Errorf("version body after reopen: %+v %v", def, err)
	}
	done, err := st2.GetJob("done")
	if err != nil || done.Status != StatusSucceeded || done.Profile == nil {
		t.Errorf("finished job after reopen: %+v %v", done, err)
	}

	// An interrupted job can be replaced by a fresh submission.
	if err := st2.CreateJob("redo", "c1", 2, "full"); err != nil {
		t.Errorf("resubmit after interruption rejected: %v", err)
	}
}
