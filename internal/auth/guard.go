package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/atmin/gitmote/internal/meta"
)

// Guard authenticates PATs and authorizes requests per-repo against the ACL
// table. It is the request guard that the transport layer consults before it
// serves a request.
type Guard struct {
	md  *meta.Metadata
	now func() time.Time // injectable clock for token expiry (time.Now in prod)
}

// NewGuard returns a Guard backed by the metadata layer.
func NewGuard(md *meta.Metadata) *Guard { return &Guard{md: md, now: time.Now} }

// Authorize checks that the request may act on repoName at perm. Anyone may
// read a public repo without a token, and a token reads it without an ACL.
// Every other case needs a valid token and the ACL level. The returned error
// selects the response:
//
//   - ErrUnauthorized: no token or an invalid token where one is required. The
//     response is 401 with a Basic challenge.
//   - ErrForbidden: authenticated, but without the permission. The response is
//     403.
//   - meta.ErrNotFound: the repo does not exist. The response is 404.
//   - Any other error: an internal failure. The response is 500.
//
// On success it returns the authenticated user. It returns nil for an
// anonymous read of a public repo. Reads never dereference the user. Only the
// write path does.
func (g *Guard) Authorize(r *http.Request, repoName string, perm meta.Perm) (*meta.User, error) {
	ctx := r.Context()

	raw, hasToken := tokenFromRequest(r)

	repo, err := g.md.GetRepo(ctx, repoName)
	if errors.Is(err, meta.ErrNotFound) {
		// Hide the repo inventory from anonymous callers. An
		// unauthenticated request never learns whether a repo exists. It gets
		// the same 401 challenge in both cases.
		if !hasToken {
			return nil, ErrUnauthorized
		}
		return nil, err // 404 to an authenticated caller
	}
	if err != nil {
		return nil, err
	}

	// An anonymous caller may only read a public repo. Anything else needs a
	// token.
	if !hasToken {
		if perm == meta.PermRead && repo.Public() {
			return nil, nil
		}
		return nil, ErrUnauthorized
	}

	vt, err := g.verify(ctx, raw)
	if err != nil {
		return nil, err
	}

	// Token constraints apply before the ACL. A repo-scoped token reaches only
	// its one repo. A read-only token cannot perform a write or admin
	// operation, even where the owner's ACL would otherwise allow it.
	if vt.repoScope != nil && *vt.repoScope != repo.ID {
		return nil, ErrForbidden
	}
	if vt.readOnly && permRank[perm] > permRank[meta.PermRead] {
		return nil, ErrForbidden
	}

	// The repo read rule gates reading, not a specific ACL level. Anyone may
	// read a public repo, and a private repo needs any ACL. Write and admin
	// need the matching ACL level.
	if perm == meta.PermRead {
		ok, err := g.md.CanRead(ctx, repo, vt.user.ID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, ErrForbidden
		}
		return &vt.user, nil
	}

	granted, err := g.md.GetACL(ctx, repo.ID, vt.user.ID)
	if errors.Is(err, meta.ErrNotFound) {
		return nil, ErrForbidden // no ACL row means no access
	}
	if err != nil {
		return nil, err
	}
	if !allows(granted, perm) {
		return nil, ErrForbidden
	}
	return &vt.user, nil
}

// verifiedToken is a successfully authenticated token: its owner and the
// constraints that Authorize enforces.
type verifiedToken struct {
	user      meta.User
	repoScope *int64
	readOnly  bool
}

// VerifyToken resolves a raw PAT string to its owner. It is the shared
// verification path. The request guard calls it after it extracts the token
// from the Authorization header. The login form of the web UI calls it with a
// pasted token. Every authentication failure returns ErrUnauthorized. A
// database failure returns its own error, which is an internal failure.
func (g *Guard) VerifyToken(ctx context.Context, raw string) (*meta.User, error) {
	vt, err := g.verify(ctx, raw)
	if err != nil {
		return nil, err
	}
	return &vt.user, nil
}

// verify authenticates a raw token and returns its owner and constraints. It
// checks the verifier in constant time and rejects an expired token. All
// authentication failures collapse to ErrUnauthorized. So a client cannot tell
// apart "unknown token", "wrong secret" and "expired". A database error from
// the lookup or the touch is returned as is.
func (g *Guard) verify(ctx context.Context, raw string) (*verifiedToken, error) {
	selector, secret, ok := split(raw)
	if !ok {
		return nil, ErrUnauthorized
	}
	ta, err := g.md.TokenBySelector(ctx, selector)
	if errors.Is(err, meta.ErrNotFound) {
		return nil, ErrUnauthorized
	}
	if err != nil {
		return nil, err
	}
	if subtle.ConstantTimeCompare([]byte(ta.Verifier), []byte(hashSecret(secret))) != 1 {
		return nil, ErrUnauthorized
	}
	if ta.ExpiresAt != nil && !ta.ExpiresAt.After(g.now()) {
		return nil, ErrUnauthorized
	}
	if err := g.md.TouchToken(ctx, ta.TokenID); err != nil {
		return nil, err
	}
	return &verifiedToken{user: ta.User, repoScope: ta.RepoScope, readOnly: ta.ReadOnly}, nil
}

// MintScoped creates and persists a token with optional constraints:
//   - repoScope sets the repo scope. nil means all the owner's repos.
//   - readOnly denies push.
//   - expiresAt sets the expiry. Zero means the token never expires.
//
// It returns the raw token string, which is shown exactly once. It mints the
// CI clone credential and any PAT that expires or has a scope.
func (g *Guard) MintScoped(ctx context.Context, userID int64, label string, repoScope *int64, readOnly bool, expiresAt time.Time) (string, error) {
	raw, selector, verifier, err := Mint()
	if err != nil {
		return "", err
	}
	if _, err := g.md.CreateScopedToken(ctx, userID, selector, verifier, label, repoScope, readOnly, expiresAt); err != nil {
		return "", err
	}
	return raw, nil
}

// tokenFromRequest extracts a PAT from the Authorization header. It accepts a
// Bearer token, as API clients send it. It also accepts Basic credentials, the
// native HTTP auth of git. There the PAT is the password, or the username when
// the password is empty.
func tokenFromRequest(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	switch {
	case h == "":
		return "", false
	case strings.HasPrefix(h, "Bearer "):
		if tok := strings.TrimSpace(strings.TrimPrefix(h, "Bearer ")); tok != "" {
			return tok, true
		}
		return "", false
	default:
		user, pass, ok := r.BasicAuth()
		if !ok {
			return "", false
		}
		if pass != "" {
			return pass, true
		}
		if user != "" {
			return user, true
		}
		return "", false
	}
}

// permRank orders permissions as read < write < admin. A missing or unknown
// level has rank 0, which means no access.
var permRank = map[meta.Perm]int{
	meta.PermRead:  1,
	meta.PermWrite: 2,
	meta.PermAdmin: 3,
}

// allows reports whether the granted permission covers the required one.
func allows(granted, required meta.Perm) bool {
	req := permRank[required]
	return req > 0 && permRank[granted] >= req
}
