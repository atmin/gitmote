package meta

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Visibility values for repos.visibility. A public repo is readable with no
// token (clone/fetch/browse); writes are never anonymous (see auth.md).
const (
	VisibilityPrivate = "private"
	VisibilityPublic  = "public"
)

// ReservedRepoNames are the top-level global route names a repo may not take,
// so a repo can never shadow a global route. Kept here as the single source so
// routing and CreateRepo validation cannot drift (docs/architecture/urls.md).
// The `.`-prefix and bare-`-` structural rules (below, in validateRepoName)
// reserve room for future system routes without a breaking rename.
var ReservedRepoNames = map[string]bool{
	"login": true, "logout": true, "users": true, "tokens": true,
	"settings": true, "new": true, "search": true, "api": true,
	"metrics": true, "internal": true, "static": true, "healthz": true,
	"version": true,
}

// Repo is a hosted repository. DefaultBranch backs the derived HEAD (see
// storage.md — HEAD is not a stored ref). Visibility is "private" or "public".
type Repo struct {
	ID            int64
	Name          string
	DefaultBranch string
	Visibility    string
	CreatedAt     time.Time
}

// Public reports whether the repo is readable anonymously.
func (r *Repo) Public() bool { return r.Visibility == VisibilityPublic }

// normalizeRepoName strips a trailing ".git" so `clone host/<repo>.git`
// resolves to the same repo as `<repo>` (both at creation and lookup).
func normalizeRepoName(name string) string {
	return strings.TrimSuffix(name, ".git")
}

// validateRepoName enforces the flat single-segment namespace: a name is one
// path segment, not a reserved global, not starting with "." and not the bare
// "-" (nor empty). See docs/architecture/urls.md. The name is assumed already
// normalized (trailing ".git" stripped).
func validateRepoName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("repo name cannot be empty")
	case strings.Contains(name, "/"):
		return fmt.Errorf("repo name %q cannot contain %q (repos are a single path segment)", name, "/")
	case strings.HasPrefix(name, "."):
		return fmt.Errorf("repo name %q cannot start with %q", name, ".")
	case name == "-":
		return fmt.Errorf("repo name cannot be %q", "-")
	case ReservedRepoNames[name]:
		return fmt.Errorf("repo name %q is reserved", name)
	}
	return nil
}

// CreateRepo inserts a repository at visibility "private". A trailing ".git" is
// stripped and the name is validated against the flat namespace rules
// (validateRepoName). defaultBranch defaults to "main" when empty.
func (m *Metadata) CreateRepo(ctx context.Context, name, defaultBranch string) (*Repo, error) {
	name = normalizeRepoName(name)
	if err := validateRepoName(name); err != nil {
		return nil, err
	}
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	created := now()
	res, err := m.db.ExecContext(ctx,
		`INSERT INTO repos (name, default_branch, visibility, created_at) VALUES (?, ?, ?, ?)`,
		name, defaultBranch, VisibilityPrivate, created)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &Repo{
		ID:            id,
		Name:          name,
		DefaultBranch: defaultBranch,
		Visibility:    VisibilityPrivate,
		CreatedAt:     parseTime(created),
	}, nil
}

// SetVisibility sets a repository's visibility ("private" or "public"). It
// returns ErrNotFound when no repo has the given id; the CHECK constraint
// rejects any other value.
func (m *Metadata) SetVisibility(ctx context.Context, repoID int64, visibility string) error {
	res, err := m.db.ExecContext(ctx,
		`UPDATE repos SET visibility = ? WHERE id = ?`, visibility, repoID)
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

// SetDefaultBranch updates a repository's default branch (the derived HEAD). It
// returns ErrNotFound when no repo has the given id.
func (m *Metadata) SetDefaultBranch(ctx context.Context, repoID int64, branch string) error {
	res, err := m.db.ExecContext(ctx,
		`UPDATE repos SET default_branch = ? WHERE id = ?`, branch, repoID)
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

// GetRepo returns the repository named name, or ErrNotFound. A trailing ".git"
// is tolerated (stripped) so `clone host/<repo>.git` resolves.
func (m *Metadata) GetRepo(ctx context.Context, name string) (*Repo, error) {
	var (
		r  Repo
		ts string
	)
	err := m.db.QueryRowContext(ctx,
		`SELECT id, name, default_branch, visibility, created_at FROM repos WHERE name = ?`,
		normalizeRepoName(name)).
		Scan(&r.ID, &r.Name, &r.DefaultBranch, &r.Visibility, &ts)
	if isNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.CreatedAt = parseTime(ts)
	return &r, nil
}

// GetRepoByID returns the repository with the given id, or ErrNotFound.
func (m *Metadata) GetRepoByID(ctx context.Context, id int64) (*Repo, error) {
	var (
		r  Repo
		ts string
	)
	err := m.db.QueryRowContext(ctx,
		`SELECT id, name, default_branch, visibility, created_at FROM repos WHERE id = ?`, id).
		Scan(&r.ID, &r.Name, &r.DefaultBranch, &r.Visibility, &ts)
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
		`SELECT id, name, default_branch, visibility, created_at FROM repos ORDER BY name`)
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
		if err := rows.Scan(&r.ID, &r.Name, &r.DefaultBranch, &r.Visibility, &ts); err != nil {
			return nil, err
		}
		r.CreatedAt = parseTime(ts)
		repos = append(repos, r)
	}
	return repos, rows.Err()
}

// ListReposForViewer returns the repos a viewer may see, ordered by name: every
// public repo plus any private repo on which the viewer holds an ACL. userID 0
// is anonymous (public only). The visibility/ACL filter lives here, in SQL, so
// the dashboard handler cannot drift from the repo-read rule (CanRead). An admin
// sees everything — the caller uses ListRepos for that.
func (m *Metadata) ListReposForViewer(ctx context.Context, userID int64) ([]Repo, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT id, name, default_branch, visibility, created_at FROM repos r
		  WHERE r.visibility = ?
		     OR EXISTS (SELECT 1 FROM acls a WHERE a.repo_id = r.id AND a.user_id = ?)
		  ORDER BY name`, VisibilityPublic, userID)
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
		if err := rows.Scan(&r.ID, &r.Name, &r.DefaultBranch, &r.Visibility, &ts); err != nil {
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
