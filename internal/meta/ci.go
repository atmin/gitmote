package meta

import (
	"context"
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
