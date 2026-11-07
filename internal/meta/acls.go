package meta

import (
	"context"
	"errors"
)

// Perm is a repository access level, ordered read < write < admin. It matches
// the CHECK constraint on acls.perm.
type Perm string

const (
	PermRead  Perm = "read"
	PermWrite Perm = "write"
	PermAdmin Perm = "admin"
)

// SetACL grants or updates the permission of user on repo. It is an upsert.
// A second call for the same repo and user replaces the level.
func (m *Metadata) SetACL(ctx context.Context, repoID, userID int64, perm Perm) error {
	_, err := m.db.ExecContext(ctx,
		`INSERT INTO acls (repo_id, user_id, perm) VALUES (?, ?, ?)
		 ON CONFLICT(repo_id, user_id) DO UPDATE SET perm = excluded.perm`,
		repoID, userID, string(perm))
	return err
}

// ACL is a grant on a repository, joined to the grantee's handle for display.
type ACL struct {
	UserID int64
	Handle string
	Perm   Perm
}

// ListACLs returns all grants on repo, ordered by handle, for the management UI.
func (m *Metadata) ListACLs(ctx context.Context, repoID int64) ([]ACL, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT a.user_id, u.handle, a.perm
		   FROM acls a JOIN users u ON u.id = a.user_id
		  WHERE a.repo_id = ? ORDER BY u.handle`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var acls []ACL
	for rows.Next() {
		var (
			a    ACL
			perm string
		)
		if err := rows.Scan(&a.UserID, &a.Handle, &perm); err != nil {
			return nil, err
		}
		a.Perm = Perm(perm)
		acls = append(acls, a)
	}
	return acls, rows.Err()
}

// DeleteACL revokes the grant of user on repo. It is idempotent. Revoking an
// absent grant is not an error.
func (m *Metadata) DeleteACL(ctx context.Context, repoID, userID int64) error {
	_, err := m.db.ExecContext(ctx,
		`DELETE FROM acls WHERE repo_id = ? AND user_id = ?`, repoID, userID)
	return err
}

// CanRead reports whether a viewer may read repo. Anyone may read a public
// repo, including the anonymous viewer (userID 0). A private repo needs an ACL
// grant of any level: read, write or admin. The git read path and the browse
// path share this check, which respects visibility (docs/architecture/urls.md,
// docs/architecture/auth.md).
func (m *Metadata) CanRead(ctx context.Context, repo *Repo, userID int64) (bool, error) {
	if repo.Public() {
		return true, nil
	}
	if userID == 0 {
		return false, nil
	}
	_, err := m.GetACL(ctx, repo.ID, userID)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// GetACL returns the permission of user on repo. It returns ErrNotFound when
// no permission is set, which means no access.
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
