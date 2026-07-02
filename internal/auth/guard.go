package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"

	"github.com/atmin/gitmote/internal/meta"
)

// Guard authenticates PATs and authorizes requests per-repo against the ACL
// table. It is the request guard the transport layer consults before serving.
type Guard struct {
	md *meta.Metadata
}

// NewGuard returns a Guard backed by the metadata layer.
func NewGuard(md *meta.Metadata) *Guard { return &Guard{md: md} }

// Authorize verifies the request's token and checks the resulting user holds at
// least perm on repoName. The returned error selects the response:
//
//   - ErrUnauthorized — no/invalid token: 401 with a Basic challenge;
//   - ErrForbidden — authenticated but lacking the permission: 403;
//   - meta.ErrNotFound — the repo does not exist: 404;
//   - any other error — an internal failure: 500.
//
// It returns the authenticated user on success.
func (g *Guard) Authorize(r *http.Request, repoName string, perm meta.Perm) (*meta.User, error) {
	ctx := r.Context()

	user, err := g.authenticate(ctx, r)
	if err != nil {
		return nil, err
	}

	repo, err := g.md.GetRepo(ctx, repoName)
	if err != nil {
		return nil, err // meta.ErrNotFound flows through to a 404
	}

	granted, err := g.md.GetACL(ctx, repo.ID, user.ID)
	if errors.Is(err, meta.ErrNotFound) {
		return nil, ErrForbidden // no ACL row means no access
	}
	if err != nil {
		return nil, err
	}
	if !allows(granted, perm) {
		return nil, ErrForbidden
	}
	return user, nil
}

// authenticate resolves the request's PAT to its owner, or ErrUnauthorized. All
// failures collapse to ErrUnauthorized so a client cannot distinguish "unknown
// token" from "wrong secret".
func (g *Guard) authenticate(ctx context.Context, r *http.Request) (*meta.User, error) {
	raw, ok := tokenFromRequest(r)
	if !ok {
		return nil, ErrUnauthorized
	}
	return g.VerifyToken(ctx, raw)
}

// VerifyToken resolves a raw PAT string to its owner, or ErrUnauthorized. It is
// the shared verification path: the request guard calls it after extracting the
// token from the Authorization header, and the web UI's login form calls it with
// a pasted token. All failures collapse to ErrUnauthorized.
func (g *Guard) VerifyToken(ctx context.Context, raw string) (*meta.User, error) {
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
	if err := g.md.TouchToken(ctx, ta.TokenID); err != nil {
		return nil, err
	}
	return &ta.User, nil
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
