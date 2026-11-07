package meta

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ZeroSHA is the all-zero object id of git. The wire protocol uses it for a ref
// that is being created (old is zero) or deleted (new is zero). CASRef treats
// ZeroSHA and the empty string as "the ref is absent".
const ZeroSHA = "0000000000000000000000000000000000000000"

// Ref is a mutable pointer: a ref name and the object id it resolves to.
type Ref struct {
	Name      string
	SHA       string
	UpdatedAt time.Time
}

// RefUpdate is one compare-and-swap: advance Name from Old to New. Old is the
// current value that the caller expects. Set Old to ZeroSHA or "" to require
// that the ref is absent. New is the desired value. Set New to ZeroSHA or ""
// to delete the ref.
type RefUpdate struct {
	Name string
	Old  string
	New  string
}

// CASMismatchError reports that a ref did not hold the expected value, so the
// update was rejected. The whole call is rolled back.
type CASMismatchError struct {
	Name string
	Want string // expected old value, normalized ("" means "absent")
	Got  string // actual current value ("" means "absent")
}

func (e *CASMismatchError) Error() string {
	return fmt.Sprintf("meta: ref %q CAS mismatch: expected %q, found %q",
		e.Name, absentAsZero(e.Want), absentAsZero(e.Got))
}

// ListRefs returns a repository's refs ordered by name.
func (m *Metadata) ListRefs(ctx context.Context, repoID int64) ([]Ref, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT name, sha, updated_at FROM refs WHERE repo_id = ? ORDER BY name`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var refs []Ref
	for rows.Next() {
		var (
			r  Ref
			ts string
		)
		if err := rows.Scan(&r.Name, &r.SHA, &ts); err != nil {
			return nil, err
		}
		r.UpdatedAt = parseTime(ts)
		refs = append(refs, r)
	}
	return refs, rows.Err()
}

// CASRef atomically advances a single ref. It succeeds only if the ref
// currently holds old; otherwise it returns *CASMismatchError and changes
// nothing. This is the linearization point for refs
// (docs/architecture/safety.md, section 3).
func (m *Metadata) CASRef(ctx context.Context, repoID int64, name, old, new string) error {
	return m.CASRefs(ctx, repoID, []RefUpdate{{Name: name, Old: old, New: new}})
}

// CASRefs applies several ref updates as one all-or-nothing transaction. If
// the expected old value of any update does not match, it applies none of
// them. It then returns a *CASMismatchError for the first ref that does not
// match. This is the guarantee behind the atomic multi-ref push.
func (m *Metadata) CASRefs(ctx context.Context, repoID int64, updates []RefUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	ts := now()
	for _, u := range updates {
		if err := casOne(ctx, tx, repoID, u, ts); err != nil {
			return err // deferred Rollback undoes any earlier updates
		}
	}
	return tx.Commit()
}

// casOne applies one RefUpdate inside tx, enforcing the compare against the
// ref's current value.
func casOne(ctx context.Context, tx *sql.Tx, repoID int64, u RefUpdate, ts string) error {
	var current string
	err := tx.QueryRowContext(ctx,
		`SELECT sha FROM refs WHERE repo_id = ? AND name = ?`, repoID, u.Name).Scan(&current)
	if err != nil && !isNoRows(err) {
		return err
	}
	// current is "" when the ref is absent.

	want := normalizeSHA(u.Old)
	if normalizeSHA(current) != want {
		return &CASMismatchError{Name: u.Name, Want: want, Got: normalizeSHA(current)}
	}

	next := normalizeSHA(u.New)
	switch {
	case next == "": // delete
		_, err = tx.ExecContext(ctx,
			`DELETE FROM refs WHERE repo_id = ? AND name = ?`, repoID, u.Name)
	case current == "": // create
		_, err = tx.ExecContext(ctx,
			`INSERT INTO refs (repo_id, name, sha, updated_at) VALUES (?, ?, ?, ?)`,
			repoID, u.Name, next, ts)
	default: // update
		_, err = tx.ExecContext(ctx,
			`UPDATE refs SET sha = ?, updated_at = ? WHERE repo_id = ? AND name = ?`,
			next, ts, repoID, u.Name)
	}
	return err
}

// normalizeSHA collapses the two spellings of "absent" (empty and ZeroSHA) to
// the empty string. So comparisons and the create and delete branches are
// uniform.
func normalizeSHA(sha string) string {
	if sha == ZeroSHA {
		return ""
	}
	return sha
}

// absentAsZero renders "absent" as ZeroSHA for error messages.
func absentAsZero(sha string) string {
	if sha == "" {
		return ZeroSHA
	}
	return sha
}
