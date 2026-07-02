package meta

import (
	"context"
	"database/sql"
	"time"
)

// User is a forge account.
type User struct {
	ID        int64
	Handle    string
	CreatedAt time.Time
}

// Token is a stored personal access token. The raw token is never persisted —
// only Hash (see HashToken) — so it is absent here too.
type Token struct {
	ID        int64
	UserID    int64
	Label     string
	CreatedAt time.Time
	LastUsed  *time.Time
}

// CreateUser inserts a user.
func (m *Metadata) CreateUser(ctx context.Context, handle string) (*User, error) {
	created := now()
	res, err := m.db.ExecContext(ctx,
		`INSERT INTO users (handle, created_at) VALUES (?, ?)`, handle, created)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &User{ID: id, Handle: handle, CreatedAt: parseTime(created)}, nil
}

// GetUser returns the user with the given handle, or ErrNotFound.
func (m *Metadata) GetUser(ctx context.Context, handle string) (*User, error) {
	var (
		u  User
		ts string
	)
	err := m.db.QueryRowContext(ctx,
		`SELECT id, handle, created_at FROM users WHERE handle = ?`, handle).
		Scan(&u.ID, &u.Handle, &ts)
	if isNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	u.CreatedAt = parseTime(ts)
	return &u, nil
}

// CreateToken stores a personal access token for a user, keeping only the hash
// of raw. The caller keeps raw; it cannot be recovered from the database.
func (m *Metadata) CreateToken(ctx context.Context, userID int64, raw, label string) (*Token, error) {
	created := now()
	res, err := m.db.ExecContext(ctx,
		`INSERT INTO tokens (user_id, hash, label, created_at) VALUES (?, ?, ?, ?)`,
		userID, HashToken(raw), label, created)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &Token{ID: id, UserID: userID, Label: label, CreatedAt: parseTime(created)}, nil
}

// AuthenticateToken resolves a raw token to its owning user and stamps the
// token's last_used. It returns ErrNotFound when no token matches — callers
// must not distinguish "unknown token" from any other auth failure to the
// client.
func (m *Metadata) AuthenticateToken(ctx context.Context, raw string) (*User, error) {
	hash := HashToken(raw)
	var (
		u      User
		userID int64
		ts     string
	)
	err := m.db.QueryRowContext(ctx,
		`SELECT u.id, u.handle, u.created_at
		   FROM tokens t JOIN users u ON u.id = t.user_id
		  WHERE t.hash = ?`, hash).
		Scan(&userID, &u.Handle, &ts)
	if isNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	u.ID = userID
	u.CreatedAt = parseTime(ts)

	if _, err := m.db.ExecContext(ctx,
		`UPDATE tokens SET last_used = ? WHERE hash = ?`, now(), hash); err != nil {
		return nil, err
	}
	return &u, nil
}

// ListTokens returns a user's tokens (metadata only, never the hash) ordered by
// creation time.
func (m *Metadata) ListTokens(ctx context.Context, userID int64) ([]Token, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT id, user_id, label, created_at, last_used
		   FROM tokens WHERE user_id = ? ORDER BY created_at, id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tokens []Token
	for rows.Next() {
		var (
			t        Token
			label    sql.NullString
			created  string
			lastUsed sql.NullString
		)
		if err := rows.Scan(&t.ID, &t.UserID, &label, &created, &lastUsed); err != nil {
			return nil, err
		}
		t.Label = label.String
		t.CreatedAt = parseTime(created)
		if lastUsed.Valid {
			lu := parseTime(lastUsed.String)
			t.LastUsed = &lu
		}
		tokens = append(tokens, t)
	}
	return tokens, rows.Err()
}
