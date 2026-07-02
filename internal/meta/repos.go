package meta

import (
	"context"
	"time"
)

// Repo is a hosted repository. DefaultBranch backs the derived HEAD (see
// storage.md — HEAD is not a stored ref).
type Repo struct {
	ID            int64
	Name          string
	DefaultBranch string
	CreatedAt     time.Time
}

// CreateRepo inserts a repository. defaultBranch defaults to "main" when empty.
func (m *Metadata) CreateRepo(ctx context.Context, name, defaultBranch string) (*Repo, error) {
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	created := now()
	res, err := m.db.ExecContext(ctx,
		`INSERT INTO repos (name, default_branch, created_at) VALUES (?, ?, ?)`,
		name, defaultBranch, created)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &Repo{ID: id, Name: name, DefaultBranch: defaultBranch, CreatedAt: parseTime(created)}, nil
}

// GetRepo returns the repository named name, or ErrNotFound.
func (m *Metadata) GetRepo(ctx context.Context, name string) (*Repo, error) {
	var (
		r  Repo
		ts string
	)
	err := m.db.QueryRowContext(ctx,
		`SELECT id, name, default_branch, created_at FROM repos WHERE name = ?`, name).
		Scan(&r.ID, &r.Name, &r.DefaultBranch, &ts)
	if isNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.CreatedAt = parseTime(ts)
	return &r, nil
}

// ListRepos returns all repositories ordered by name.
func (m *Metadata) ListRepos(ctx context.Context) ([]Repo, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT id, name, default_branch, created_at FROM repos ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var repos []Repo
	for rows.Next() {
		var (
			r  Repo
			ts string
		)
		if err := rows.Scan(&r.ID, &r.Name, &r.DefaultBranch, &ts); err != nil {
			return nil, err
		}
		r.CreatedAt = parseTime(ts)
		repos = append(repos, r)
	}
	return repos, rows.Err()
}

// parseTime parses an RFC 3339 timestamp written by now(); a parse failure
// yields the zero time rather than an error, since the column is always
// written by this package.
func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
