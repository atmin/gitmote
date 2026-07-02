package meta

import "context"

// Perm is a repository access level, ordered read < write < admin. It matches
// the CHECK constraint on acls.perm.
type Perm string

const (
	PermRead  Perm = "read"
	PermWrite Perm = "write"
	PermAdmin Perm = "admin"
)

// SetACL grants (or updates) user's permission on repo. It is an upsert: a
// second call for the same (repo, user) replaces the level.
func (m *Metadata) SetACL(ctx context.Context, repoID, userID int64, perm Perm) error {
	_, err := m.db.ExecContext(ctx,
		`INSERT INTO acls (repo_id, user_id, perm) VALUES (?, ?, ?)
		 ON CONFLICT(repo_id, user_id) DO UPDATE SET perm = excluded.perm`,
		repoID, userID, string(perm))
	return err
}

// GetACL returns user's permission on repo, or ErrNotFound when none is set
// (no access).
func (m *Metadata) GetACL(ctx context.Context, repoID, userID int64) (Perm, error) {
	var perm string
	err := m.db.QueryRowContext(ctx,
		`SELECT perm FROM acls WHERE repo_id = ? AND user_id = ?`, repoID, userID).
		Scan(&perm)
	if isNoRows(err) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return Perm(perm), nil
}
