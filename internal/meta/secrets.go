package meta

import (
	"context"
	"fmt"
	"time"
)

// CISecret is one repo's stored secret envelope. It never carries the plaintext
// value — only the sealed bytes and the key version that sealed them. Decryption
// happens in internal/secrets with the server-held master key.
type CISecret struct {
	Name    string
	Version int
	IV      []byte
	CT      []byte
}

// SetSecret upserts a repo's secret envelope by (repo_id, name). Written only by
// the leader, like every other row.
func (m *Metadata) SetSecret(ctx context.Context, repoID int64, name string, version int, iv, ct []byte) error {
	_, err := m.db.ExecContext(ctx,
		`INSERT INTO ci_secrets (repo_id, name, v, iv, ct, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(repo_id, name) DO UPDATE SET
		   v = excluded.v, iv = excluded.iv, ct = excluded.ct, created_at = excluded.created_at`,
		repoID, name, version, iv, ct, now())
	return err
}

// ListSecretNames returns a repo's secret names (never values), ordered by name
// — the only view the UI ever gets.
func (m *Metadata) ListSecretNames(ctx context.Context, repoID int64) ([]string, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT name FROM ci_secrets WHERE repo_id = ? ORDER BY name`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, rows.Err()
}

// GetSecrets returns a repo's secret envelopes for decryption at trigger time.
func (m *Metadata) GetSecrets(ctx context.Context, repoID int64) ([]CISecret, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT name, v, iv, ct FROM ci_secrets WHERE repo_id = ? ORDER BY name`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var secrets []CISecret
	for rows.Next() {
		var s CISecret
		if err := rows.Scan(&s.Name, &s.Version, &s.IV, &s.CT); err != nil {
			return nil, err
		}
		secrets = append(secrets, s)
	}
	return secrets, rows.Err()
}

// DeleteSecret removes a repo's secret by name. Deleting an absent secret is a
// no-op (idempotent), so it returns no ErrNotFound.
func (m *Metadata) DeleteSecret(ctx context.Context, repoID int64, name string) error {
	_, err := m.db.ExecContext(ctx,
		`DELETE FROM ci_secrets WHERE repo_id = ? AND name = ?`, repoID, name)
	return err
}

// GetOrCreateSecret returns the persisted server secret named name, generating
// and persisting it with gen the first time it's requested. It backs the
// auto-provisioned session cookie key and CI worker secret (see cmd/gitmote): an
// absent value is created once, by the writer. A follower cannot write, and on a
// fresh storage root a missing secret may only mean that another instance, booting
// at the same moment, created it after this one restored its snapshot. So a
// follower catches up from the replica until the secret appears, or until it takes
// the lease and creates the secret itself, for at most secretWaitTimeout.
// Narrowly scoped to these named server secrets, not a general key–value store.
func (m *Metadata) GetOrCreateSecret(ctx context.Context, name string, gen func() ([]byte, error)) ([]byte, error) {
	val, err := m.awaitSecret(ctx, name)
	if err == nil {
		return val, nil
	}
	if !isNoRows(err) {
		return nil, err
	}
	val, err = gen()
	if err != nil {
		return nil, err
	}
	// ON CONFLICT DO NOTHING keeps a concurrent creator's value; the re-read below
	// then returns whichever insert won, so every caller agrees on one value.
	if _, err := m.db.ExecContext(ctx,
		`INSERT INTO server_secrets (name, value) VALUES (?, ?)
		 ON CONFLICT(name) DO NOTHING`, name, val); err != nil {
		return nil, err
	}
	return m.getSecret(ctx, name)
}

// secretWaitTimeout bounds how long a follower waits for a missing server secret
// to reach it, or for the lease to create it. A peer that created the secret ships
// it within a second, and an idle peer yields its lease within seconds.
const secretWaitTimeout = 30 * time.Second

// secretRetryInterval spaces a follower's catch-up attempts for a missing secret.
const secretRetryInterval = 500 * time.Millisecond

// awaitSecret reads the secret named name. On the writer (an unleased sole writer
// included) it returns the read as is, sql.ErrNoRows when the secret is absent. On
// a follower a missing secret is retried: catch up from the replica, read again,
// and try to take the lease, until the secret appears (returned), this instance
// becomes the writer (sql.ErrNoRows, so the caller creates it), or
// secretWaitTimeout passes.
func (m *Metadata) awaitSecret(ctx context.Context, name string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, secretWaitTimeout)
	defer cancel()
	for {
		val, err := m.getSecret(ctx, name)
		if err == nil || !isNoRows(err) || m.db.IsLeader() {
			return val, err
		}
		if _, err := m.db.Refresh(ctx); err != nil {
			return nil, fmt.Errorf("catch up for server secret %q: %w", name, err)
		}
		if val, err := m.getSecret(ctx, name); err == nil || !isNoRows(err) {
			return val, err
		}
		if _, err := m.db.TryPromote(ctx); err != nil {
			return nil, fmt.Errorf("take the lease for server secret %q: %w", name, err)
		}
		if m.db.IsLeader() {
			continue // read once more as the writer, then create if still absent
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("server secret %q: not on the replica, and the lease is held elsewhere: %w", name, ctx.Err())
		case <-time.After(secretRetryInterval):
		}
	}
}

// getSecret reads the secret named name; sql.ErrNoRows when it is absent.
func (m *Metadata) getSecret(ctx context.Context, name string) ([]byte, error) {
	var val []byte
	err := m.db.QueryRowContext(ctx,
		`SELECT value FROM server_secrets WHERE name = ?`, name).Scan(&val)
	return val, err
}
