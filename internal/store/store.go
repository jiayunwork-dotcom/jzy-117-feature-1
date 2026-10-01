// Package store persists canal lines, their version history, profile
// computation jobs and finished profiles.
//
// Storage choice: SQLite through modernc.org/sqlite (a pure-Go driver, no
// cgo, so the Docker build keeps CGO_ENABLED=0 and stays a static binary on
// the golang:1.22 image). SQLite keeps the whole durable state in one file on
// the mounted data directory; it gives us real transactions (needed for the
// optimistic-concurrency version append and for writing a job and its result
// atomically), a partial unique index (at most one active job per
// channel/version) and checkpointed WAL durability, without standing up a
// separate database service for a single-container deployment.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"openchannel/internal/channel"
	"openchannel/internal/profile"
)

// Job statuses.
const (
	StatusPending     = "pending"
	StatusRunning     = "running"
	StatusSucceeded   = "succeeded"
	StatusFailed      = "failed"
	StatusCancelled   = "cancelled"
	StatusInterrupted = "interrupted"
)

// ErrNotFound is returned for unknown channel/version/job references.
var ErrNotFound = errors.New("store: not found")

// ErrVersionConflict is returned when an update is based on a stale version
// (someone else already appended a newer version).
var ErrVersionConflict = errors.New("store: version conflict")

// ErrActiveJob means a running/pending job already exists for the target
// channel/version.
var ErrActiveJob = errors.New("store: an active job already exists for this version")

// ProfileDocument is a finished profile plus the bookkeeping attached to it.
type ProfileDocument struct {
	Profile    *profile.Profile        `json:"profile"`
	Failure    *profile.ComputeFailure `json:"failure,omitempty"`
	Recompute  string                  `json:"recompute_mode,omitempty"` // full / partial
	Comparison *profile.Comparison     `json:"comparison,omitempty"`
	JobID      string                  `json:"job_id"`
	ComputedAt string                  `json:"computed_at"`
}

// Job is the stored state of one asynchronous profile computation.
type Job struct {
	ID         string
	ChannelID  string
	Version    int
	Status     string
	Progress   float64
	Recompute  string
	Error      string
	Failure    *profile.ComputeFailure
	CreatedAt  string
	UpdatedAt  string
	StartedAt  string
	FinishedAt string
	Profile    *ProfileDocument // populated for the terminal-success state
}

// Store is the durable service state.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at path. The parent
// directory is created if necessary, so pointing the service at a freshly
// mounted empty data directory works without extra setup.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("creating data directory %q: %w", dir, err)
		}
	}
	// Single connection serialises writers and matches SQLite's file model;
	// the workload is tiny. WAL + busy timeout avoid lock surprises.
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(schema)
	return err
}

const schema = `
CREATE TABLE IF NOT EXISTS channels (
	id         TEXT PRIMARY KEY,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS versions (
	channel_id TEXT NOT NULL REFERENCES channels(id),
	version    INTEGER NOT NULL,
	definition TEXT NOT NULL,
	created_at TEXT NOT NULL,
	PRIMARY KEY (channel_id, version)
);
CREATE TABLE IF NOT EXISTS jobs (
	id          TEXT PRIMARY KEY,
	channel_id  TEXT NOT NULL,
	version     INTEGER NOT NULL,
	status      TEXT NOT NULL,
	progress    REAL NOT NULL DEFAULT 0,
	recompute   TEXT NOT NULL DEFAULT 'full',
	error       TEXT NOT NULL DEFAULT '',
	failure     TEXT NOT NULL DEFAULT '',
	started_at  TEXT,
	finished_at TEXT,
	created_at  TEXT NOT NULL,
	updated_at  TEXT NOT NULL,
	result      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_jobs_channel_version ON jobs(channel_id, version, created_at);
-- At most one job in an active state per channel/version. Terminal states are
-- excluded, so retrying after failure/cancellation is allowed.
CREATE UNIQUE INDEX IF NOT EXISTS uq_active_job
	ON jobs(channel_id, version)
	WHERE status IN ('pending','running');
CREATE INDEX IF NOT EXISTS idx_versions_channel ON versions(channel_id, version);
`

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// CreateChannel stores a new channel together with version 1. The whole insert
// is one transaction.
func (s *Store) CreateChannel(id string, def channel.Definition) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	ts := now()
	if _, err := tx.Exec(
		`INSERT INTO channels(id, created_at, updated_at) VALUES(?,?,?)`, id, ts, ts); err != nil {
		return err
	}
	if err := insertVersion(tx, id, 1, def, ts); err != nil {
		return err
	}
	return tx.Commit()
}

func insertVersion(tx *sql.Tx, channelID string, version int, def channel.Definition, ts string) error {
	body, err := json.Marshal(def)
	if err != nil {
		return err
	}
	_, err = tx.Exec(
		`INSERT INTO versions(channel_id, version, definition, created_at) VALUES(?,?,?,?)`,
		channelID, version, string(body), ts)
	return err
}

// ChannelExists reports whether id is present.
func (s *Store) ChannelExists(id string) (bool, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM channels WHERE id = ?`, id).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// LatestVersion returns the highest stored version number.
func (s *Store) LatestVersion(channelID string) (int, error) {
	var v int
	err := s.db.QueryRow(
		`SELECT COALESCE(MAX(version),0) FROM versions WHERE channel_id = ?`, channelID).Scan(&v)
	if err != nil {
		return 0, err
	}
	if v == 0 {
		return 0, ErrNotFound
	}
	return v, nil
}

// UpdateChannel appends a new version. It implements optimistic concurrency:
// the caller states the version it edited (baseVersion); if a newer version
// already exists, ErrVersionConflict is returned and nothing is written.
//
// On success every finished result of older versions is marked stale.
func (s *Store) UpdateChannel(channelID string, baseVersion int, def channel.Definition) (newVersion int, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	var latest int
	if err := tx.QueryRow(
		`SELECT COALESCE(MAX(version),0) FROM versions WHERE channel_id = ?`, channelID).
		Scan(&latest); err != nil {
		return 0, err
	}
	if latest == 0 {
		return 0, ErrNotFound
	}
	if baseVersion != latest {
		return 0, fmt.Errorf("%w: expected base version %d, latest is %d",
			ErrVersionConflict, latest, baseVersion)
	}

	newVersion = latest + 1
	ts := now()
	if err := insertVersion(tx, channelID, newVersion, def, ts); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(
		`UPDATE channels SET updated_at = ? WHERE id = ?`, ts, channelID); err != nil {
		return 0, err
	}
	// Staleness of old results is derived at read time (version < latest);
	// stored job rows are immutable history and are not rewritten here.
	return newVersion, tx.Commit()
}

// GetDefinition reads one version's definition.
func (s *Store) GetDefinition(channelID string, version int) (channel.Definition, error) {
	var body string
	err := s.db.QueryRow(
		`SELECT definition FROM versions WHERE channel_id = ? AND version = ?`,
		channelID, version).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return channel.Definition{}, ErrNotFound
	}
	if err != nil {
		return channel.Definition{}, err
	}
	var def channel.Definition
	if err := json.Unmarshal([]byte(body), &def); err != nil {
		return channel.Definition{}, err
	}
	return def, nil
}

// VersionInfo is one row of a channel's version history.
type VersionInfo struct {
	Version   int    `json:"version"`
	CreatedAt string `json:"created_at"`
}

// ListVersions returns the version history oldest-first.
func (s *Store) ListVersions(channelID string) ([]VersionInfo, error) {
	rows, err := s.db.Query(
		`SELECT version, created_at FROM versions WHERE channel_id = ? ORDER BY version`, channelID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []VersionInfo
	for rows.Next() {
		var v VersionInfo
		if err := rows.Scan(&v.Version, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if out == nil {
		return nil, ErrNotFound
	}
	return out, nil
}

// ListChannels returns the stored channel ids oldest-first.
func (s *Store) ListChannels() ([]string, error) {
	rows, err := s.db.Query(`SELECT id FROM channels ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
