package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"openchannel/internal/profile"
)

// CreateJob inserts a pending job. If a pending/running job already exists for
// the same channel/version it returns ErrActiveJob (enforced by a partial
// unique index, so the guarantee survives concurrent callers and restarts).
func (s *Store) CreateJob(id, channelID string, version int, recompute string) error {
	ts := now()
	_, err := s.db.Exec(
		`INSERT INTO jobs(id, channel_id, version, status, progress, recompute, created_at, updated_at)
		 VALUES(?,?,?,?,0,?,?,?)`,
		id, channelID, version, StatusPending, recompute, ts, ts)
	if err != nil && isUniqueConstraint(err) {
		return ErrActiveJob
	}
	return err
}

// ActiveJob returns the pending/running job for a channel/version, or
// ErrNotFound if none exists.
func (s *Store) ActiveJob(channelID string, version int) (*Job, error) {
	return s.queryJob(
		`SELECT `+jobColumns+` FROM jobs
		 WHERE channel_id = ? AND version = ? AND status IN (?,?)
		 ORDER BY created_at DESC LIMIT 1`,
		channelID, version, StatusPending, StatusRunning)
}

// GetJob fetches a job by id.
func (s *Store) GetJob(id string) (*Job, error) {
	return s.queryJob(`SELECT `+jobColumns+` FROM jobs WHERE id = ?`, id)
}

// LatestJobForVersion returns the most recently created job for a version.
func (s *Store) LatestJobForVersion(channelID string, version int) (*Job, error) {
	return s.queryJob(
		`SELECT `+jobColumns+` FROM jobs WHERE channel_id = ? AND version = ?
		 ORDER BY created_at DESC LIMIT 1`,
		channelID, version)
}

// MarkRunning moves a pending job to running. The status guard makes stale
// workers (e.g. after a cancel that won the race) unable to resurrect it.
func (s *Store) MarkRunning(id string) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE jobs SET status = ?, started_at = COALESCE(started_at, ?), updated_at = ?
		 WHERE id = ? AND status IN (?,?)`,
		StatusRunning, now(), now(), id, StatusPending, StatusRunning)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// UpdateProgress records progress for a job that is still active.
func (s *Store) UpdateProgress(id string, frac float64) error {
	_, err := s.db.Exec(
		`UPDATE jobs SET progress = ?, updated_at = ?
		 WHERE id = ? AND status IN (?,?)`,
		frac, now(), id, StatusPending, StatusRunning)
	return err
}

// FinishSucceeded stores the profile and flips the job to succeeded in one
// transaction, so readers can never observe a terminal status with a missing
// profile or a profile under a non-terminal status.
func (s *Store) FinishSucceeded(id string, doc *ProfileDocument) error {
	body, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	ts := now()
	res, err := s.db.Exec(
		`UPDATE jobs SET status = ?, progress = 1, result = ?, recompute = ?, finished_at = ?, updated_at = ?
		 WHERE id = ? AND status IN (?,?)`,
		StatusSucceeded, string(body), doc.Recompute, ts, ts, id, StatusPending, StatusRunning)
	if err != nil {
		return err
	}
	return requireOne(res)
}

// FinishFailed records a hydraulic failure or a generic execution error.
func (s *Store) FinishFailed(id string, failure *profile.ComputeFailure, errMsg string) error {
	var failureJSON string
	if failure != nil {
		b, err := json.Marshal(failure)
		if err != nil {
			return err
		}
		failureJSON = string(b)
	}
	ts := now()
	res, err := s.db.Exec(
		`UPDATE jobs SET status = ?, failure = ?, error = ?, finished_at = ?, updated_at = ?
		 WHERE id = ? AND status IN (?,?)`,
		StatusFailed, failureJSON, errMsg, ts, ts, id, StatusPending, StatusRunning)
	if err != nil {
		return err
	}
	return requireOne(res)
}

// Cancel transitions an active job to cancelled. Returns true if this call
// performed the transition; false if the job was already terminal.
func (s *Store) Cancel(id string) (bool, error) {
	ts := now()
	res, err := s.db.Exec(
		`UPDATE jobs SET status = ?, finished_at = ?, updated_at = ?
		 WHERE id = ? AND status IN (?,?)`,
		StatusCancelled, ts, ts, id, StatusPending, StatusRunning)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// RecoverInterrupted marks every still-active job as interrupted. Called once
// at startup: a profile step is cheap and stateless, and rather than silently
// resuming a job whose worker died with the process, we surface the state and
// let the caller resubmit. This guarantees no job is stuck at
// running/pending forever after a restart.
func (s *Store) RecoverInterrupted() (int64, error) {
	res, err := s.db.Exec(
		`UPDATE jobs SET status = ?, finished_at = ?, updated_at = ?
		 WHERE status IN (?,?)`,
		StatusInterrupted, now(), now(), StatusPending, StatusRunning)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

const jobColumns = `id, channel_id, version, status, progress, recompute, error, failure,
	COALESCE(started_at,''), COALESCE(finished_at,''), created_at, updated_at, result`

func (s *Store) queryJob(query string, args ...any) (*Job, error) {
	row := s.db.QueryRow(query, args...)
	var (
		j                       Job
		failureJSON, resultJSON string
	)
	err := row.Scan(&j.ID, &j.ChannelID, &j.Version, &j.Status, &j.Progress,
		&j.Recompute, &j.Error, &failureJSON, &j.StartedAt, &j.FinishedAt,
		&j.CreatedAt, &j.UpdatedAt, &resultJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if failureJSON != "" {
		j.Failure = &profile.ComputeFailure{}
		if err := json.Unmarshal([]byte(failureJSON), j.Failure); err != nil {
			return nil, fmt.Errorf("decoding failure of job %s: %w", j.ID, err)
		}
	}
	if resultJSON != "" {
		j.Profile = &ProfileDocument{}
		if err := json.Unmarshal([]byte(resultJSON), j.Profile); err != nil {
			return nil, fmt.Errorf("decoding result of job %s: %w", j.ID, err)
		}
	}
	return &j, nil
}

func requireOne(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("store: job transition affected no rows (already terminal)")
	}
	return nil
}

func isUniqueConstraint(err error) bool {
	return err != nil && strings.Contains(err.Error(), "constraint failed")
}
