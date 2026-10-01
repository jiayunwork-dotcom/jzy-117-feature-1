package store

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
)

func TestVersionChainAndOptimisticLock(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	spec := json.RawMessage(`{"flow":5}`)

	c, err := s.CreateChannel("ch1", "line", spec, "initial")
	if err != nil {
		t.Fatal(err)
	}
	if c.CurrentVersion() != 1 {
		t.Fatalf("current = %d", c.CurrentVersion())
	}

	// Stale base version rejected.
	if _, err := s.AddVersion("ch1", 1, spec, ""); err != nil {
		t.Fatalf("first v2 attempt should succeed: %v", err)
	}
	_, err = s.AddVersion("ch1", 1, spec, "")
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("want ErrVersionConflict, got %v", err)
	}

	// v3 on the new current works.
	c, err = s.AddVersion("ch1", 2, spec, "")
	if err != nil {
		t.Fatalf("v3: %v", err)
	}
	if c.CurrentVersion() != 3 {
		t.Fatalf("current = %d, want 3", c.CurrentVersion())
	}

	if _, err := s.GetVersion("ch1", 2); err != nil {
		t.Fatalf("old version still retrievable: %v", err)
	}
	if _, err := s.GetChannel("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want not found, got %v", err)
	}
}

func TestRestartLoadsChannelsAndJobs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateChannel("ch1", "", json.RawMessage(`{}`), ""); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateJob(&Job{ID: "j1", ChannelID: "ch1", Version: 1, Status: JobSucceeded, Progress: 1,
		Result: json.RawMessage(`{"ok":true}`)}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateJob(&Job{ID: "j2", ChannelID: "ch1", Version: 1, Status: JobRunning, Progress: 0.4}); err != nil {
		t.Fatal(err)
	}

	// Reopen as after a container restart.
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	c, err := s2.GetChannel("ch1")
	if err != nil || c.CurrentVersion() != 1 {
		t.Fatalf("channel after restart: %v %v", c, err)
	}
	j1, err := s2.GetJob("j1")
	if err != nil || j1.Status != JobSucceeded || string(j1.Result) == "" {
		t.Fatalf("finished job after restart: %v %v", j1, err)
	}
	recovered, err := s2.RecoverInterrupted()
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 1 || recovered[0].ID != "j2" || recovered[0].Status != JobInterrupted {
		t.Fatalf("recovery = %+v", recovered)
	}
	// Recovery is idempotent.
	if again, err := s2.RecoverInterrupted(); err != nil || len(again) != 0 {
		t.Fatalf("second recovery = %d, %v", len(again), err)
	}
}

func TestResultStalenessDerived(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	spec := json.RawMessage(`{}`)
	if _, err := s.CreateChannel("ch1", "", spec, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateJob(&Job{ID: "j1", ChannelID: "ch1", Version: 1, Status: JobSucceeded}); err != nil {
		t.Fatal(err)
	}
	j, _ := s.LatestSuccess("ch1", 1)
	if j.ResultStale {
		t.Fatal("fresh result flagged stale")
	}
	if _, err := s.AddVersion("ch1", 1, spec, ""); err != nil {
		t.Fatal(err)
	}
	j, _ = s.LatestSuccess("ch1", 1)
	if !j.ResultStale {
		t.Fatal("v1 result must be stale after v2")
	}
}
