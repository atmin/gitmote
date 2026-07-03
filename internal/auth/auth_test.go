package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/atmin/gitmote/internal/meta"
)

func openMeta(t *testing.T) *meta.Metadata {
	t.Helper()
	m, err := meta.Open(context.Background(), meta.Config{
		LocalPath: filepath.Join(t.TempDir(), "meta.sqlite3"),
	})
	if err != nil {
		t.Fatalf("meta.Open: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func TestMintRoundTrips(t *testing.T) {
	raw, selector, verifier, err := Mint()
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	gotSel, gotSecret, ok := split(raw)
	if !ok {
		t.Fatalf("split(%q) failed", raw)
	}
	if gotSel != selector {
		t.Errorf("selector = %q, want %q", gotSel, selector)
	}
	if hashSecret(gotSecret) != verifier {
		t.Error("hashSecret(secret) != verifier")
	}
	// Two mints never collide.
	raw2, _, _, _ := Mint()
	if raw2 == raw {
		t.Error("two Mint calls produced the same token")
	}
}

func TestSplitRejectsMalformed(t *testing.T) {
	for _, raw := range []string{"", "nope", "gmt_", "gmt_onlyselector", "gmt_.secret", "gmt_sel.", "sel.secret"} {
		if _, _, ok := split(raw); ok {
			t.Errorf("split(%q) = ok, want rejected", raw)
		}
	}
}

// seedToken creates a user + token and returns the raw token and user id.
func seedToken(t *testing.T, m *meta.Metadata) (string, int64) {
	t.Helper()
	ctx := context.Background()
	u, err := m.CreateUser(ctx, "atmin")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	raw, selector, verifier, err := Mint()
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, err := m.CreateToken(ctx, u.ID, selector, verifier, "test"); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	return raw, u.ID
}

// bearer builds a request carrying repoName and an optional bearer token.
func bearer(token string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

func TestAuthorize(t *testing.T) {
	ctx := context.Background()
	m := openMeta(t)
	g := NewGuard(m)

	repo, err := m.CreateRepo(ctx, "atmin/repo", "main")
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	raw, userID := seedToken(t, m)

	// No token / malformed / unknown / wrong secret → ErrUnauthorized.
	sel, _, _ := split(raw)
	unauth := map[string]*http.Request{
		"no token":     bearer(""),
		"malformed":    bearer("not-a-token"),
		"unknown":      bearer("gmt_ffffffffffffffffffffffffffffffff.1234"),
		"wrong secret": bearer("gmt_" + sel + ".wrongsecret"),
	}
	for name, r := range unauth {
		if _, err := g.Authorize(r, "atmin/repo", meta.PermRead); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("%s: Authorize = %v, want ErrUnauthorized", name, err)
		}
	}

	// Valid token, no ACL → ErrForbidden.
	if _, err := g.Authorize(bearer(raw), "atmin/repo", meta.PermRead); !errors.Is(err, ErrForbidden) {
		t.Errorf("no ACL: Authorize = %v, want ErrForbidden", err)
	}

	// Valid token, unknown repo → meta.ErrNotFound.
	if _, err := g.Authorize(bearer(raw), "no/such", meta.PermRead); !errors.Is(err, meta.ErrNotFound) {
		t.Errorf("unknown repo: Authorize = %v, want meta.ErrNotFound", err)
	}

	// Grant read: read is allowed, write is not.
	if err := m.SetACL(ctx, repo.ID, userID, meta.PermRead); err != nil {
		t.Fatalf("SetACL: %v", err)
	}
	user, err := g.Authorize(bearer(raw), "atmin/repo", meta.PermRead)
	if err != nil {
		t.Fatalf("read with read ACL: %v", err)
	}
	if user.ID != userID {
		t.Errorf("authorized user = %d, want %d", user.ID, userID)
	}
	if _, err := g.Authorize(bearer(raw), "atmin/repo", meta.PermWrite); !errors.Is(err, ErrForbidden) {
		t.Errorf("write with read ACL: Authorize = %v, want ErrForbidden", err)
	}

	// Admin covers read.
	if err := m.SetACL(ctx, repo.ID, userID, meta.PermAdmin); err != nil {
		t.Fatalf("SetACL admin: %v", err)
	}
	if _, err := g.Authorize(bearer(raw), "atmin/repo", meta.PermRead); err != nil {
		t.Errorf("read with admin ACL: %v", err)
	}

	// A successful authorization stamps last_used.
	toks, _ := m.ListTokens(ctx, userID)
	if len(toks) != 1 || toks[0].LastUsed == nil {
		t.Errorf("last_used not stamped after Authorize: %+v", toks)
	}
}

func TestVerifyToken(t *testing.T) {
	ctx := context.Background()
	m := openMeta(t)
	g := NewGuard(m)
	raw, userID := seedToken(t, m)
	sel, _, _ := split(raw)

	// Valid raw token resolves to its owner — the web UI login path.
	user, err := g.VerifyToken(ctx, raw)
	if err != nil {
		t.Fatalf("VerifyToken(valid): %v", err)
	}
	if user.ID != userID {
		t.Errorf("user = %d, want %d", user.ID, userID)
	}

	// Malformed, unknown selector, and wrong secret all → ErrUnauthorized.
	for name, tok := range map[string]string{
		"malformed":    "not-a-token",
		"unknown":      "gmt_ffffffffffffffffffffffffffffffff.1234",
		"wrong secret": "gmt_" + sel + ".wrongsecret",
	} {
		if _, err := g.VerifyToken(ctx, tok); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("%s: VerifyToken = %v, want ErrUnauthorized", name, err)
		}
	}
}

func TestBasicAuthCredentials(t *testing.T) {
	ctx := context.Background()
	m := openMeta(t)
	g := NewGuard(m)
	repo, _ := m.CreateRepo(ctx, "atmin/repo", "main")
	raw, userID := seedToken(t, m)
	if err := m.SetACL(ctx, repo.ID, userID, meta.PermRead); err != nil {
		t.Fatalf("SetACL: %v", err)
	}

	// Token as Basic password (git's default) and as Basic username (token-only)
	// both authenticate.
	for _, creds := range []struct {
		user, pass string
	}{
		{"git", raw}, // password carries the token
		{raw, ""},    // username carries the token
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.SetBasicAuth(creds.user, creds.pass)
		if _, err := g.Authorize(r, "atmin/repo", meta.PermRead); err != nil {
			t.Errorf("Basic(%q,%q): Authorize = %v, want ok", creds.user, creds.pass, err)
		}
	}
}

func TestTokenExpiry(t *testing.T) {
	ctx := context.Background()
	m := openMeta(t)
	g := NewGuard(m)
	repo, _ := m.CreateRepo(ctx, "atmin/repo", "main")
	u, _ := m.CreateUser(ctx, "atmin")
	if err := m.SetACL(ctx, repo.ID, u.ID, meta.PermRead); err != nil {
		t.Fatalf("SetACL: %v", err)
	}

	expiry := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	raw, err := g.MintScoped(ctx, u.ID, "ci", nil, false, expiry)
	if err != nil {
		t.Fatalf("MintScoped: %v", err)
	}

	// Before expiry: authorizes.
	g.now = func() time.Time { return expiry.Add(-time.Minute) }
	if _, err := g.Authorize(bearer(raw), "atmin/repo", meta.PermRead); err != nil {
		t.Errorf("before expiry: Authorize = %v, want ok", err)
	}

	// At/after expiry: rejected as unauthorized.
	g.now = func() time.Time { return expiry }
	if _, err := g.Authorize(bearer(raw), "atmin/repo", meta.PermRead); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("at expiry: Authorize = %v, want ErrUnauthorized", err)
	}
	g.now = func() time.Time { return expiry.Add(time.Hour) }
	if _, err := g.VerifyToken(ctx, raw); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("after expiry: VerifyToken = %v, want ErrUnauthorized", err)
	}
}

func TestRepoScope(t *testing.T) {
	ctx := context.Background()
	m := openMeta(t)
	g := NewGuard(m)
	repoA, _ := m.CreateRepo(ctx, "atmin/a", "main")
	repoB, _ := m.CreateRepo(ctx, "atmin/b", "main")
	u, _ := m.CreateUser(ctx, "atmin")
	// The owner has read on BOTH repos.
	if err := m.SetACL(ctx, repoA.ID, u.ID, meta.PermRead); err != nil {
		t.Fatalf("SetACL a: %v", err)
	}
	if err := m.SetACL(ctx, repoB.ID, u.ID, meta.PermRead); err != nil {
		t.Fatalf("SetACL b: %v", err)
	}

	// A token scoped to A authorizes A but is denied B despite the ACL on B.
	raw, err := g.MintScoped(ctx, u.ID, "scoped-a", &repoA.ID, false, time.Time{})
	if err != nil {
		t.Fatalf("MintScoped: %v", err)
	}
	if _, err := g.Authorize(bearer(raw), "atmin/a", meta.PermRead); err != nil {
		t.Errorf("scoped repo: Authorize = %v, want ok", err)
	}
	if _, err := g.Authorize(bearer(raw), "atmin/b", meta.PermRead); !errors.Is(err, ErrForbidden) {
		t.Errorf("other repo: Authorize = %v, want ErrForbidden", err)
	}
}

func TestReadOnlyToken(t *testing.T) {
	ctx := context.Background()
	m := openMeta(t)
	g := NewGuard(m)
	repo, _ := m.CreateRepo(ctx, "atmin/repo", "main")
	u, _ := m.CreateUser(ctx, "atmin")
	// The owner can write, but the token is read-only.
	if err := m.SetACL(ctx, repo.ID, u.ID, meta.PermWrite); err != nil {
		t.Fatalf("SetACL: %v", err)
	}

	raw, err := g.MintScoped(ctx, u.ID, "ro", nil, true, time.Time{})
	if err != nil {
		t.Fatalf("MintScoped: %v", err)
	}
	if _, err := g.Authorize(bearer(raw), "atmin/repo", meta.PermRead); err != nil {
		t.Errorf("read with read-only token: Authorize = %v, want ok", err)
	}
	if _, err := g.Authorize(bearer(raw), "atmin/repo", meta.PermWrite); !errors.Is(err, ErrForbidden) {
		t.Errorf("write with read-only token: Authorize = %v, want ErrForbidden", err)
	}
}

func TestUnscopedTokenUnaffected(t *testing.T) {
	ctx := context.Background()
	m := openMeta(t)
	g := NewGuard(m)
	repo, _ := m.CreateRepo(ctx, "atmin/repo", "main")
	raw, userID := seedToken(t, m) // ordinary CreateToken PAT
	if err := m.SetACL(ctx, repo.ID, userID, meta.PermWrite); err != nil {
		t.Fatalf("SetACL: %v", err)
	}

	// No expiry, any owned repo, read + write per ACL — even far in the future.
	g.now = func() time.Time { return time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC) }
	if _, err := g.Authorize(bearer(raw), "atmin/repo", meta.PermWrite); err != nil {
		t.Errorf("unscoped token write: Authorize = %v, want ok", err)
	}
}

func TestAllows(t *testing.T) {
	cases := []struct {
		granted, required meta.Perm
		want              bool
	}{
		{meta.PermRead, meta.PermRead, true},
		{meta.PermWrite, meta.PermRead, true},
		{meta.PermAdmin, meta.PermWrite, true},
		{meta.PermRead, meta.PermWrite, false},
		{meta.PermWrite, meta.PermAdmin, false},
		{"", meta.PermRead, false},
		{meta.PermRead, "", false},
	}
	for _, c := range cases {
		if got := allows(c.granted, c.required); got != c.want {
			t.Errorf("allows(%q, %q) = %v, want %v", c.granted, c.required, got, c.want)
		}
	}
}
