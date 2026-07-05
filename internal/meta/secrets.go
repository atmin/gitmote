package meta

import (
	"context"
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
// absent value is created once — on the very first, uncontended cold start, which
// is necessarily the leader (a follower opens read-only, so its insert would fail
// loud, and a restart then reads the leader's value restored from the replica).
// Narrowly scoped to these named server secrets, not a general key–value store.
func (m *Metadata) GetOrCreateSecret(ctx context.Context, name string, gen func() ([]byte, error)) ([]byte, error) {
	var val []byte
	err := m.db.QueryRowContext(ctx,
		`SELECT value FROM server_secrets WHERE name = ?`, name).Scan(&val)
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
	if err := m.db.QueryRowContext(ctx,
		`SELECT value FROM server_secrets WHERE name = ?`, name).Scan(&val); err != nil {
		return nil, err
	}
	return val, nil
}
