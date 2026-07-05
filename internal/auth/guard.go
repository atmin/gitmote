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
// table. It is the request guard the transport layer consults before serving.
type Guard struct {
	md  *meta.Metadata
	now func() time.Time // injectable clock for token expiry (time.Now in prod)
}

// NewGuard returns a Guard backed by the metadata layer.
func NewGuard(md *meta.Metadata) *Guard { return &Guard{md: md, now: time.Now} }

// Authorize checks the request may act on repoName at perm. A public repo is
// readable with no token; every other case needs a valid token plus the ACL
// level. The returned error selects the response:
//
//   - ErrUnauthorized — no/invalid token where one is required: 401 with a Basic challenge;
//   - ErrForbidden — authenticated but lacking the permission: 403;
//   - meta.ErrNotFound — the repo does not exist: 404;
//   - any other error — an internal failure: 500.
//
// On success it returns the authenticated user, or nil for an anonymous read of
// a public repo (reads never dereference the user; only the write path does).
func (g *Guard) Authorize(r *http.Request, repoName string, perm meta.Perm) (*meta.User, error) {
	ctx := r.Context()

	raw, hasToken := tokenFromRequest(r)

	repo, err := g.md.GetRepo(ctx, repoName)
	if errors.Is(err, meta.ErrNotFound) {
		// Hide a private forge's repo inventory from anonymous callers: an
		// unauthenticated request never learns whether a repo exists (it gets the
		// same 401 challenge whether or not it does).
		if !hasToken {
			return nil, ErrUnauthorized
		}
		return nil, err // 404 to an authenticated caller
	}
	if err != nil {
		return nil, err
	}

	// Anonymous: allowed only for reading a public repo; anything else needs a token.
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

	// Token constraints gate before the ACL: a repo-scoped token reaches only its
	// one repo, and a read-only token cannot perform a write/admin operation —
	// even where the owner's ACL would otherwise allow it.
	if vt.repoScope != nil && *vt.repoScope != repo.ID {
		return nil, ErrForbidden
	}
	if vt.readOnly && permRank[perm] > permRank[meta.PermRead] {
		return nil, ErrForbidden
	}

	// Reading is gated by repo-read (public → anyone, private → any ACL), not by a
	// specific ACL level; write/admin require the matching ACL level.
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

// verifiedToken is a successfully authenticated token: its owner plus the
// constraints Authorize enforces.
type verifiedToken struct {
	user      meta.User
	repoScope *int64
	readOnly  bool
}

// VerifyToken resolves a raw PAT string to its owner, or ErrUnauthorized. It is
// the shared verification path: the request guard calls it after extracting the
// token from the Authorization header, and the web UI's login form calls it with
// a pasted token. All failures collapse to ErrUnauthorized.
func (g *Guard) VerifyToken(ctx context.Context, raw string) (*meta.User, error) {
	vt, err := g.verify(ctx, raw)
	if err != nil {
		return nil, err
	}
	return &vt.user, nil
}

// verify authenticates a raw token and returns its owner and constraints. It
// checks the verifier in constant time and rejects an expired token. All
// failures collapse to ErrUnauthorized so a client cannot distinguish "unknown
// token" from "wrong secret" from "expired".
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

// MintScoped creates and persists a token with optional constraints — repoScope
// (nil = all the owner's repos), readOnly (deny push), expiresAt (zero = never) —
// and returns the raw token string, shown exactly once. It is the CI clone
// credential mint (task 21) and any expiring/scoped PAT.
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
// Bearer token (API clients) and Basic credentials (git's native HTTP auth,
// where the PAT is sent as the password — or the username when the password is
// empty).
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

// permRank orders permissions read < write < admin; a missing/unknown level is
// rank 0 (no access).
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
