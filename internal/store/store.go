// Package store is the service's persistence layer: an embedded JSON-file
// store on a single mounted data directory.
//
// Why files rather than SQLite or another third-party database:
//
//   - The workload is a single container, a handful of channels and
//     occasional profile jobs; throughput and concurrent writers are not
//     concerns. A process-wide RWMutex plus one file per entity is simpler
//     to reason about than a connection pool and migrations.
//   - Zero third-party dependencies keeps the image build reproducible
//     without a module proxy and the binary CGO-free (the existing
//     Dockerfile sets CGO_ENABLED=0; pulling in cgo SQLite would break that
//     build), while the data remains human-readable for inspection/backup.
//
// Durability: every write goes to a temp file in the same directory, is
// fsynced, and then atomically renamed over the target (rename is atomic on
// POSIX). A reader therefore never observes a half-written file. All writes
// happen under the store mutex.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// ErrNotFound is returned for unknown channel/job IDs or version numbers.
var ErrNotFound = errors.New("store: not found")

// ErrVersionConflict is returned when an update names a base version that
// is not the channel's current (latest) version: someone else committed a
// newer version in between.
var ErrVersionConflict = errors.New("store: version conflict")

// Version is one immutable snapshot of a channel line.
type Version struct {
	Number    int       `json:"number"` // 1-based, monotonic within the channel
	CreatedAt time.Time `json:"created_at"`
	Note      string    `json:"note,omitempty"`
	// Spec serialises the profile.Spec; the store stays free of hydraulic
	// packages by keeping it as raw JSON.
	Spec json.RawMessage `json:"spec"`
}

// Channel is the aggregate root: metadata plus its version chain.
type Channel struct {
	ID        string    `json:"id"`
	Name      string    `json:"name,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	Versions  []Version `json:"versions"`
}

// CurrentVersion returns the latest version number.
func (c *Channel) CurrentVersion() int {
	return c.Versions[len(c.Versions)-1].Number
}

// Job statuses.
const (
	JobQueued      = "queued"
	JobRunning     = "running"
	JobSucceeded   = "succeeded"
	JobFailed      = "failed" // hydraulic failure: steep reach / critical crossing
	JobCanceled    = "canceled"
	JobInterrupted = "interrupted" // process died before a terminal state was reached
)

// Job is an asynchronous profile push. Result holds the stored result
// document for succeeded jobs.
type Job struct {
	ID          string          `json:"id"`
	ChannelID   string          `json:"channel_id"`
	Version     int             `json:"version"`
	Status      string          `json:"status"`
	Progress    float64         `json:"progress"` // 0..1
	Error       string          `json:"error,omitempty"`
	Failure     json.RawMessage `json:"failure,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	StartedAt   *time.Time      `json:"started_at,omitempty"`
	FinishedAt  *time.Time      `json:"finished_at,omitempty"`
	ResultStale bool            `json:"result_stale"` // derived: a newer channel version exists
}

// Store owns the on-disk data directory.
type Store struct {
	mu   sync.RWMutex
	dir  string
	jobs map[string]*Job
}

// Open (re)opens a data directory, creating it if necessary, and loads all
// channel and job documents into memory.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "channels"), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "jobs"), 0o755); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, jobs: map[string]*Job{}}

	if err := s.loadJobs(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) channelsDir() string { return filepath.Join(s.dir, "channels") }
func (s *Store) jobsDir() string     { return filepath.Join(s.dir, "jobs") }

// Dir returns the data directory (used by restart tests).
func (s *Store) Dir() string { return s.dir }

// --- channels ---

// CreateChannel writes a brand-new channel with version 1.
func (s *Store) CreateChannel(id, name string, spec []byte, note string) (*Channel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	c := &Channel{
		ID:        id,
		Name:      name,
		CreatedAt: now,
		Versions: []Version{{
			Number: 1, CreatedAt: now, Note: note,
			Spec: append(json.RawMessage(nil), spec...),
		}},
	}
	if err := s.writeChannelLocked(c); err != nil {
		return nil, err
	}
	return cloneChannel(c), nil
}

// AddVersion appends a new version. baseVersion must equal the current
// latest version, otherwise ErrVersionConflict is returned and nothing is
// written (optimistic concurrency).
func (s *Store) AddVersion(channelID string, baseVersion int, spec []byte, note string) (*Channel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.readChannelLocked(channelID)
	if err != nil {
		return nil, err
	}
	if baseVersion != c.CurrentVersion() {
		return nil, fmt.Errorf("%w: base version %d is not current (%d)",
			ErrVersionConflict, baseVersion, c.CurrentVersion())
	}
	now := time.Now().UTC()
	c.Versions = append(c.Versions, Version{
		Number: c.CurrentVersion() + 1, CreatedAt: now, Note: note,
		Spec: append(json.RawMessage(nil), spec...),
	})
	if err := s.writeChannelLocked(c); err != nil {
		return nil, err
	}
	return cloneChannel(c), nil
}

// GetChannel returns the channel aggregate.
func (s *Store) GetChannel(id string) (*Channel, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, err := s.readChannelLocked(id)
	if err != nil {
		return nil, err
	}
	return cloneChannel(c), nil
}

// Rename updates the cosmetic channel name.
func (s *Store) Rename(id, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.readChannelLocked(id)
	if err != nil {
		return err
	}
	c.Name = name
	return s.writeChannelLocked(c)
}

// ListChannels returns all stored channels, sorted by creation time.
func (s *Store) ListChannels() ([]*Channel, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entries, err := os.ReadDir(s.channelsDir())
	if err != nil {
		return nil, err
	}
	out := make([]*Channel, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		id := e.Name()[:len(e.Name())-len(".json")]
		c, err := s.readChannelLocked(id)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// GetVersion returns one version document.
func (s *Store) GetVersion(channelID string, number int) (*Version, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, err := s.readChannelLocked(channelID)
	if err != nil {
		return nil, err
	}
	for i := range c.Versions {
		if c.Versions[i].Number == number {
			v := c.Versions[i]
			return &v, nil
		}
	}
	return nil, fmt.Errorf("%w: channel %s version %d", ErrNotFound, channelID, number)
}

func (s *Store) readChannelLocked(id string) (*Channel, error) {
	path := filepath.Join(s.channelsDir(), id+".json")
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: channel %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	var c Channel
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("channel %s: %w", id, err)
	}
	return &c, nil
}

func (s *Store) writeChannelLocked(c *Channel) error {
	return writeJSONAtomic(filepath.Join(s.channelsDir(), c.ID+".json"), c)
}

// --- jobs ---

// CreateJob persists a new queued job.
func (s *Store) CreateJob(j *Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if j.ID == "" {
		return errors.New("store: job requires an id")
	}
	now := time.Now().UTC()
	j.CreatedAt = now
	j.UpdatedAt = now
	if j.Status == "" {
		j.Status = JobQueued
	}
	if err := s.writeJobLocked(j); err != nil {
		return err
	}
	s.jobs[j.ID] = j
	return nil
}

// UpdateJob mutates a job under the lock and persists it. The callback gets
// the stored job to edit; a returned error aborts the update.
func (s *Store) UpdateJob(id string, fn func(*Job) error) (*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return nil, fmt.Errorf("%w: job %s", ErrNotFound, id)
	}
	if err := fn(j); err != nil {
		return nil, err
	}
	j.UpdatedAt = time.Now().UTC()
	if err := s.writeJobLocked(j); err != nil {
		return nil, err
	}
	cp := *j
	return &cp, nil
}

// GetJob returns a copy of a job.
func (s *Store) GetJob(id string) (*Job, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	j, ok := s.jobs[id]
	if !ok {
		return nil, fmt.Errorf("%w: job %s", ErrNotFound, id)
	}
	cp := *j
	s.decorateStaleLocked(&cp)
	return &cp, nil
}

// LatestSuccess returns the most recent succeeded job for a channel version.
func (s *Store) LatestSuccess(channelID string, version int) (*Job, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var found *Job
	for _, j := range s.jobs {
		if j.ChannelID == channelID && j.Version == version && j.Status == JobSucceeded {
			if found == nil || j.CreatedAt.After(found.CreatedAt) {
				found = j
			}
		}
	}
	if found == nil {
		return nil, fmt.Errorf("%w: no succeeded job for %s v%d", ErrNotFound, channelID, version)
	}
	cp := *found
	s.decorateStaleLocked(&cp)
	return &cp, nil
}

// ActiveJob returns the queued/running job for a channel version, if any.
func (s *Store) ActiveJob(channelID string, version int) *Job {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, j := range s.jobs {
		if j.ChannelID == channelID && j.Version == version &&
			(j.Status == JobQueued || j.Status == JobRunning) {
			cp := *j
			return &cp
		}
	}
	return nil
}

// RecoverInterrupted marks every queued/running job as interrupted after a
// process (re)start. A crashed run cannot be resumed mid-march, so the job
// is terminal and the caller may submit again.
func (s *Store) RecoverInterrupted() ([]*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var recovered []*Job
	for _, j := range s.jobs {
		if j.Status == JobQueued || j.Status == JobRunning {
			now := time.Now().UTC()
			j.Status = JobInterrupted
			j.Progress = 0
			j.Error = "the service restarted before this job finished; submit a new profile job"
			j.FinishedAt = &now
			j.UpdatedAt = now
			if err := s.writeJobLocked(j); err != nil {
				return nil, err
			}
			cp := *j
			recovered = append(recovered, &cp)
		}
	}
	return recovered, nil
}

// decorateStaleLocked flags succeeded results whose channel has a newer
// version. The flag is derived, never stored.
func (s *Store) decorateStaleLocked(j *Job) {
	j.ResultStale = false
	if j.Status != JobSucceeded {
		return
	}
	if c, err := s.readChannelLocked(j.ChannelID); err == nil && c.CurrentVersion() > j.Version {
		j.ResultStale = true
	}
}

func (s *Store) loadJobs() error {
	entries, err := os.ReadDir(s.jobsDir())
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.jobsDir(), e.Name()))
		if err != nil {
			return err
		}
		var j Job
		if err := json.Unmarshal(b, &j); err != nil {
			return fmt.Errorf("job %s: %w", e.Name(), err)
		}
		s.jobs[j.ID] = &j
	}
	return nil
}

func (s *Store) writeJobLocked(j *Job) error {
	return writeJSONAtomic(filepath.Join(s.jobsDir(), j.ID+".json"), j)
}

func cloneChannel(c *Channel) *Channel {
	cp := *c
	cp.Versions = append([]Version(nil), c.Versions...)
	return &cp
}

// writeJSONAtomic writes via temp file + fsync + rename in the same
// directory, guaranteeing readers never see a torn document.
func writeJSONAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
