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

// Token is a stored personal access token's metadata. Neither the raw token nor
// its verifier is exposed here — this is what the UI lists.
type Token struct {
	ID        int64
	UserID    int64
	Label     string
	CreatedAt time.Time
	LastUsed  *time.Time
}

// TokenAuth is a token's stored verifier plus its owner, returned to the auth
// layer for constant-time verification (see internal/auth).
type TokenAuth struct {
	TokenID  int64
	Verifier string
	User     User
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

// CreateToken stores a personal access token for a user. selector is the token's
// public lookup key; verifier is the hash of its secret half (see internal/auth
// for the token format). Neither the raw token nor the secret is persisted.
func (m *Metadata) CreateToken(ctx context.Context, userID int64, selector, verifier, label string) (*Token, error) {
	created := now()
	res, err := m.db.ExecContext(ctx,
		`INSERT INTO tokens (user_id, selector, verifier, label, created_at) VALUES (?, ?, ?, ?, ?)`,
		userID, selector, verifier, label, created)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &Token{ID: id, UserID: userID, Label: label, CreatedAt: parseTime(created)}, nil
}

// TokenBySelector returns the verifier and owner of the token with the given
// selector, or ErrNotFound. The auth layer compares the verifier in constant
// time; this method deliberately does not touch last_used (see TouchToken).
func (m *Metadata) TokenBySelector(ctx context.Context, selector string) (*TokenAuth, error) {
	var (
		ta TokenAuth
		ts string
	)
	err := m.db.QueryRowContext(ctx,
		`SELECT t.id, t.verifier, u.id, u.handle, u.created_at
		   FROM tokens t JOIN users u ON u.id = t.user_id
		  WHERE t.selector = ?`, selector).
		Scan(&ta.TokenID, &ta.Verifier, &ta.User.ID, &ta.User.Handle, &ts)
	if isNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	ta.User.CreatedAt = parseTime(ts)
	return &ta, nil
}

// TouchToken stamps a token's last_used with the current time. Called after a
// successful verification.
func (m *Metadata) TouchToken(ctx context.Context, tokenID int64) error {
	_, err := m.db.ExecContext(ctx,
		`UPDATE tokens SET last_used = ? WHERE id = ?`, now(), tokenID)
	return err
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
