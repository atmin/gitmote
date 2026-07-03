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
