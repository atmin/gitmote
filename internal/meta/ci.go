package meta

import (
	"context"
	"database/sql"
	"time"
)

// RunStatus is the lifecycle state of a CI run. A run starts queued and moves
// to a terminal state as later stages execute it.
type RunStatus string

const (
	RunQueued     RunStatus = "queued"
	RunRunning    RunStatus = "running"
	RunPassed     RunStatus = "passed"
	RunFailed     RunStatus = "failed"
	RunError      RunStatus = "error"
	RunSuperseded RunStatus = "superseded"
)

// Run is one CI run: a workflow triggered by a ref advancing to SHA. Rows are
// written only by the leader, like refs.
type Run struct {
	ID        int64
	RepoID    int64
	Ref       string
	SHA       string
	Status    RunStatus
	CreatedAt time.Time
	UpdatedAt time.Time
}

// CreateRun records a queued run for a ref advance and returns it.
func (m *Metadata) CreateRun(ctx context.Context, repoID int64, ref, sha string) (*Run, error) {
	ts := now()
	res, err := m.db.ExecContext(ctx,
		`INSERT INTO ci_runs (repo_id, ref, sha, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		repoID, ref, sha, string(RunQueued), ts, ts)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &Run{
		ID:        id,
		RepoID:    repoID,
		Ref:       ref,
		SHA:       sha,
		Status:    RunQueued,
		CreatedAt: parseTime(ts),
		UpdatedAt: parseTime(ts),
	}, nil
}

// SetRunStatus transitions a run to status. It returns ErrNotFound when no run
// has the given id.
func (m *Metadata) SetRunStatus(ctx context.Context, runID int64, status RunStatus) error {
	res, err := m.db.ExecContext(ctx,
		`UPDATE ci_runs SET status = ?, updated_at = ? WHERE id = ?`,
		string(status), now(), runID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetRun returns the run with the given id, or ErrNotFound.
func (m *Metadata) GetRun(ctx context.Context, runID int64) (*Run, error) {
	var (
		r      Run
		status string
		cts    string
		uts    string
	)
	err := m.db.QueryRowContext(ctx,
		`SELECT id, repo_id, ref, sha, status, created_at, updated_at
		 FROM ci_runs WHERE id = ?`, runID).
		Scan(&r.ID, &r.RepoID, &r.Ref, &r.SHA, &status, &cts, &uts)
	if isNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.Status = RunStatus(status)
	r.CreatedAt = parseTime(cts)
	r.UpdatedAt = parseTime(uts)
	return &r, nil
}

// ListRuns returns a repository's runs, newest first, capped at limit (<= 0
// means no limit).
func (m *Metadata) ListRuns(ctx context.Context, repoID int64, limit int) ([]Run, error) {
	query := `SELECT id, repo_id, ref, sha, status, created_at, updated_at
	          FROM ci_runs WHERE repo_id = ? ORDER BY id DESC`
	args := []any{repoID}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := m.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var runs []Run
	for rows.Next() {
		var (
			r      Run
			status string
			cts    string
			uts    string
		)
		if err := rows.Scan(&r.ID, &r.RepoID, &r.Ref, &r.SHA, &status, &cts, &uts); err != nil {
			return nil, err
		}
		r.Status = RunStatus(status)
		r.CreatedAt = parseTime(cts)
		r.UpdatedAt = parseTime(uts)
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// Job is one unit of a run: a single workflow file. Name is the workflow file's
// base name (stage 2); LogKey is the ci/ object key, set on completion (stage 4).
type Job struct {
	ID        int64
	RunID     int64
	Name      string
	Status    RunStatus
	LogKey    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// CreateJob records a queued job under a run and returns it.
func (m *Metadata) CreateJob(ctx context.Context, runID int64, name string) (*Job, error) {
	ts := now()
	res, err := m.db.ExecContext(ctx,
		`INSERT INTO ci_jobs (run_id, name, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?)`,
		runID, name, string(RunQueued), ts, ts)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &Job{
		ID:        id,
		RunID:     runID,
		Name:      name,
		Status:    RunQueued,
		CreatedAt: parseTime(ts),
		UpdatedAt: parseTime(ts),
	}, nil
}

// SetJobStatus transitions a job to status. It returns ErrNotFound when no job
// has the given id.
func (m *Metadata) SetJobStatus(ctx context.Context, jobID int64, status RunStatus) error {
	res, err := m.db.ExecContext(ctx,
		`UPDATE ci_jobs SET status = ?, updated_at = ? WHERE id = ?`,
		string(status), now(), jobID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetJob returns the job with the given id, or ErrNotFound.
func (m *Metadata) GetJob(ctx context.Context, jobID int64) (*Job, error) {
	var (
		j      Job
		status string
		logKey sql.NullString
		cts    string
		uts    string
	)
	err := m.db.QueryRowContext(ctx,
		`SELECT id, run_id, name, status, log_key, created_at, updated_at
		   FROM ci_jobs WHERE id = ?`, jobID).
		Scan(&j.ID, &j.RunID, &j.Name, &status, &logKey, &cts, &uts)
	if isNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	j.Status = RunStatus(status)
	j.LogKey = logKey.String
	j.CreatedAt = parseTime(cts)
	j.UpdatedAt = parseTime(uts)
	return &j, nil
}

// ClaimJob atomically transitions a queued job to running and returns it. It
// returns ErrNotFound when no job with the id is queued — already claimed,
// terminal, or absent — so a concurrent double-claim yields exactly one winner.
func (m *Metadata) ClaimJob(ctx context.Context, jobID int64) (*Job, error) {
	res, err := m.db.ExecContext(ctx,
		`UPDATE ci_jobs SET status = ?, updated_at = ? WHERE id = ? AND status = ?`,
		string(RunRunning), now(), jobID, string(RunQueued))
	if err != nil {
		return nil, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, ErrNotFound
	}
	return m.GetJob(ctx, jobID)
}

// SetJobResult records a job's terminal status and its log key together. It
// returns ErrNotFound when no job has the given id.
func (m *Metadata) SetJobResult(ctx context.Context, jobID int64, status RunStatus, logKey string) error {
	res, err := m.db.ExecContext(ctx,
		`UPDATE ci_jobs SET status = ?, log_key = ?, updated_at = ? WHERE id = ?`,
		string(status), logKey, now(), jobID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SweepStuckJobs marks every running job last updated before cutoff as error and
// returns the ids it changed, so the caller can roll up their runs. A runner that
// dies without completing would otherwise leave a job running forever.
func (m *Metadata) SweepStuckJobs(ctx context.Context, cutoff time.Time) ([]int64, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT id, updated_at FROM ci_jobs WHERE status = ?`, string(RunRunning))
	if err != nil {
		return nil, err
	}
	var stuck []int64
	for rows.Next() {
		var (
			id  int64
			uts string
		)
		if err := rows.Scan(&id, &uts); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if parseTime(uts).Before(cutoff) {
			stuck = append(stuck, id)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()

	for _, id := range stuck {
		if _, err := m.db.ExecContext(ctx,
			`UPDATE ci_jobs SET status = ?, updated_at = ? WHERE id = ? AND status = ?`,
			string(RunError), now(), id, string(RunRunning)); err != nil {
			return nil, err
		}
	}
	return stuck, nil
}

// ListJobs returns a run's jobs ordered by creation (id).
func (m *Metadata) ListJobs(ctx context.Context, runID int64) ([]Job, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT id, run_id, name, status, log_key, created_at, updated_at
		   FROM ci_jobs WHERE run_id = ? ORDER BY id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []Job
	for rows.Next() {
		var (
			j      Job
			status string
			logKey sql.NullString
			cts    string
			uts    string
		)
		if err := rows.Scan(&j.ID, &j.RunID, &j.Name, &status, &logKey, &cts, &uts); err != nil {
			return nil, err
		}
		j.Status = RunStatus(status)
		j.LogKey = logKey.String
		j.CreatedAt = parseTime(cts)
		j.UpdatedAt = parseTime(uts)
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}
