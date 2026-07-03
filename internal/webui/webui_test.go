package webui

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/atmin/gitmote/internal/auth"
	"github.com/atmin/gitmote/internal/meta"
)

// harness wires a real meta DB + auth guard behind the UI handler, seeds one
// global admin with a token, and returns the admin's raw token for login.
type harness struct {
	t     *testing.T
	h     *Handler
	md    *meta.Metadata
	mux   *http.ServeMux
	admin *meta.User
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	md, err := meta.Open(ctx, meta.Config{LocalPath: filepath.Join(t.TempDir(), "meta.sqlite3")})
	if err != nil {
		t.Fatalf("meta.Open: %v", err)
	}
	t.Cleanup(func() { _ = md.Close() })

	h, err := New(md, auth.NewGuard(md), []byte("test-cookie-key"), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	h.Register(mux)

	admin, err := md.CreateAdmin(ctx, "root")
	if err != nil {
		t.Fatalf("CreateAdmin: %v", err)
	}
	return &harness{t: t, h: h, md: md, mux: mux, admin: admin}
}

// mintTokenFor mints a token for an existing user and returns the raw string.
func (x *harness) mintTokenFor(userID int64) string {
	x.t.Helper()
	raw, sel, ver, err := auth.Mint()
	if err != nil {
		x.t.Fatalf("Mint: %v", err)
	}
	if _, err := x.md.CreateToken(context.Background(), userID, sel, ver, "test"); err != nil {
		x.t.Fatalf("CreateToken: %v", err)
	}
	return raw
}

func (x *harness) do(method, target string, form url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
	x.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, target, body)
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	x.mux.ServeHTTP(rec, r)
	return rec
}

// login posts the admin token and returns the resulting session cookie.
func (x *harness) login(raw string) *http.Cookie {
	x.t.Helper()
	rec := x.do(http.MethodPost, "/login", url.Values{"token": {raw}}, nil)
	if rec.Code != http.StatusSeeOther {
		x.t.Fatalf("login status = %d, want 303 (body: %s)", rec.Code, rec.Body)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieName {
			return c
		}
	}
	x.t.Fatal("login set no session cookie")
	return nil
}

func TestGoldenPath(t *testing.T) {
	x := newHarness(t)
	ctx := context.Background()
	session := x.login(x.mintTokenFor(x.admin.ID))

	// Create a user to own a repo and receive grants/tokens.
	if rec := x.do(http.MethodPost, "/ui/users", url.Values{"handle": {"alice"}}, session); rec.Code != http.StatusOK {
		t.Fatalf("create user: %d (%s)", rec.Code, rec.Body)
	}
	if _, err := x.md.GetUser(ctx, "alice"); err != nil {
		t.Fatalf("alice not created: %v", err)
	}

	// Create a repo owned by alice.
	rec := x.do(http.MethodPost, "/ui/repos",
		url.Values{"owner": {"alice"}, "name": {"app"}, "default_branch": {"main"}}, session)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "created alice/app") {
		t.Fatalf("create repo: %d (%s)", rec.Code, rec.Body)
	}
	repo, err := x.md.GetRepo(ctx, "alice/app")
	if err != nil {
		t.Fatalf("repo not created: %v", err)
	}

	// Set default branch.
	rec = x.do(http.MethodPost, "/ui/repos/default-branch",
		url.Values{"repo": {"alice/app"}, "default_branch": {"trunk"}}, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("set default branch: %d (%s)", rec.Code, rec.Body)
	}
	if got, _ := x.md.GetRepo(ctx, "alice/app"); got.DefaultBranch != "trunk" {
		t.Errorf("default branch = %q, want trunk", got.DefaultBranch)
	}

	// Mint a token for alice — the raw token is shown exactly once in the body.
	rec = x.do(http.MethodPost, "/ui/tokens", url.Values{"user": {"alice"}, "label": {"laptop"}}, session)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "gmt_") {
		t.Fatalf("mint token: %d (%s)", rec.Code, rec.Body)
	}
	alice, _ := x.md.GetUser(ctx, "alice")
	toks, _ := x.md.ListTokens(ctx, alice.ID)
	if len(toks) != 1 {
		t.Fatalf("alice tokens = %d, want 1", len(toks))
	}

	// Grant write to alice on her repo.
	rec = x.do(http.MethodPost, "/ui/acls",
		url.Values{"repo": {"alice/app"}, "handle": {"alice"}, "perm": {"write"}}, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("grant acl: %d (%s)", rec.Code, rec.Body)
	}
	if perm, err := x.md.GetACL(ctx, repo.ID, alice.ID); err != nil || perm != meta.PermWrite {
		t.Errorf("GetACL = %q, %v; want write", perm, err)
	}

	// Revoke it again.
	rec = x.do(http.MethodPost, "/ui/acls/revoke",
		url.Values{"repo": {"alice/app"}, "user_id": {itoa(alice.ID)}}, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke acl: %d (%s)", rec.Code, rec.Body)
	}
	if _, err := x.md.GetACL(ctx, repo.ID, alice.ID); err == nil {
		t.Error("acl still present after revoke")
	}
}

// TestCreateRepoGrantsOwnerAdmin: creating a repo through the UI grants the owner
// admin on it, so it is immediately usable (clone/push) without a separate ACL
// step — the gap that left a freshly created repo 403-ing every push.
func TestCreateRepoGrantsOwnerAdmin(t *testing.T) {
	x := newHarness(t)
	ctx := context.Background()
	session := x.login(x.mintTokenFor(x.admin.ID))

	if rec := x.do(http.MethodPost, "/ui/users", url.Values{"handle": {"bob"}}, session); rec.Code != http.StatusOK {
		t.Fatalf("create user: %d (%s)", rec.Code, rec.Body)
	}
	if rec := x.do(http.MethodPost, "/ui/repos",
		url.Values{"owner": {"bob"}, "name": {"proj"}, "default_branch": {"main"}}, session); rec.Code != http.StatusOK {
		t.Fatalf("create repo: %d (%s)", rec.Code, rec.Body)
	}

	bob, _ := x.md.GetUser(ctx, "bob")
	repo, err := x.md.GetRepo(ctx, "bob/proj")
	if err != nil {
		t.Fatalf("repo not created: %v", err)
	}
	if perm, err := x.md.GetACL(ctx, repo.ID, bob.ID); err != nil || perm != meta.PermAdmin {
		t.Errorf("owner ACL after create = %q, %v; want admin", perm, err)
	}
}

func TestUnauthenticatedDenied(t *testing.T) {
	x := newHarness(t)

	// GET without a session → redirect to login.
	rec := x.do(http.MethodGet, "/ui/repos", nil, nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Errorf("GET /ui/repos unauth = %d loc %q, want 303 /login", rec.Code, rec.Header().Get("Location"))
	}

	// POST without a session → 401 (not a browser navigation).
	rec = x.do(http.MethodPost, "/ui/repos",
		url.Values{"owner": {"root"}, "name": {"x"}}, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("POST /ui/repos unauth = %d, want 401", rec.Code)
	}
}

func TestNonAdminDenied(t *testing.T) {
	x := newHarness(t)
	ctx := context.Background()

	// A regular user with a valid token cannot log in to the UI.
	bob, _ := x.md.CreateUser(ctx, "bob")
	bobToken := x.mintTokenFor(bob.ID)
	if rec := x.do(http.MethodPost, "/login", url.Values{"token": {bobToken}}, nil); rec.Code != http.StatusForbidden {
		t.Errorf("non-admin login = %d, want 403", rec.Code)
	}

	// Even a validly-signed session for a non-admin is rejected by the guard
	// (covers demotion mid-session): forge bob's cookie and hit an admin page.
	forged := x.sessionFor(bob.ID)
	if rec := x.do(http.MethodGet, "/ui/repos", nil, forged); rec.Code != http.StatusForbidden {
		t.Errorf("non-admin session = %d, want 403", rec.Code)
	}
}

func TestCreateRepoRejectsReservedAndUnknownOwner(t *testing.T) {
	x := newHarness(t)
	session := x.login(x.mintTokenFor(x.admin.ID))

	// Reserved top-level owner would shadow a route.
	rec := x.do(http.MethodPost, "/ui/repos", url.Values{"owner": {"ui"}, "name": {"x"}}, session)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "reserved") {
		t.Errorf("reserved owner: %d (%s)", rec.Code, rec.Body)
	}

	// Owner must be an existing user.
	rec = x.do(http.MethodPost, "/ui/repos", url.Values{"owner": {"ghost"}, "name": {"x"}}, session)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "no such user") {
		t.Errorf("unknown owner: %d (%s)", rec.Code, rec.Body)
	}
	if _, err := x.md.GetRepo(context.Background(), "ghost/x"); err == nil {
		t.Error("repo created despite unknown owner")
	}
}

// sessionFor forges a valid signed session cookie for userID, used to test the
// middleware's admin re-check independent of the login form.
func (x *harness) sessionFor(userID int64) *http.Cookie {
	x.t.Helper()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	x.h.sess.issue(rec, r, userID, x.h.now())
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieName {
			return c
		}
	}
	x.t.Fatal("issue set no cookie")
	return nil
}

func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}
