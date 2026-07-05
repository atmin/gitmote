package webui

import (
	"context"
	"encoding/base64"
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
	"github.com/atmin/gitmote/internal/repo"
	"github.com/atmin/gitmote/internal/secrets"
	"github.com/atmin/gitmote/internal/store"
)

// harness wires a real meta DB + auth guard behind the UI handler, seeds one
// global admin with a token, and returns the admin's raw token for login.
type harness struct {
	t     *testing.T
	h     *Handler
	md    *meta.Metadata
	store store.Store
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

	objs := store.NewMem()
	mz := repo.New(md, objs, t.TempDir())

	// A keyring with one all-zero key: enough to exercise the secrets panel
	// (Enabled() true, encrypt/decrypt round-trip) without a real key.
	t.Setenv("GITMOTE_CI_SECRET_KEY_V1", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	kr, err := secrets.NewKeyringFromEnv()
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	svc := secrets.NewService(kr, md)

	h, err := New(md, mz, objs, auth.NewGuard(md), svc, []byte("test-cookie-key"), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	h.Register(mux)

	admin, err := md.CreateAdmin(ctx, "root")
	if err != nil {
		t.Fatalf("CreateAdmin: %v", err)
	}
	return &harness{t: t, h: h, md: md, store: objs, mux: mux, admin: admin}
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

// TestRootDashboard: the bare root renders the viewer-scoped dashboard (not a
// redirect, not a 404). It matches only the exact root, leaving repo paths to
// the git handler.
func TestRootDashboard(t *testing.T) {
	x := newHarness(t)
	rec := x.do(http.MethodGet, "/", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Repositories") {
		t.Errorf("dashboard body missing heading:\n%s", rec.Body)
	}
	// Anonymous sees a sign-in link, not admin nav.
	if !strings.Contains(rec.Body.String(), `href="/login"`) {
		t.Errorf("anonymous dashboard missing sign-in link:\n%s", rec.Body)
	}
}

// TestDashboardScoping: the dashboard lists exactly the repos a viewer may see —
// anonymous → public only; an ACL user → their repos + public; admin → all.
func TestDashboardScoping(t *testing.T) {
	x := newHarness(t)
	ctx := context.Background()

	pub, _ := x.md.CreateRepo(ctx, "pub", "main")
	if err := x.md.SetVisibility(ctx, pub.ID, meta.VisibilityPublic); err != nil {
		t.Fatal(err)
	}
	if _, err := x.md.CreateRepo(ctx, "priv", "main"); err != nil { // admin-only
		t.Fatal(err)
	}
	mine, _ := x.md.CreateRepo(ctx, "mine", "main") // granted to alice
	alice, _ := x.md.CreateUser(ctx, "alice")
	if err := x.md.SetACL(ctx, mine.ID, alice.ID, meta.PermRead); err != nil {
		t.Fatal(err)
	}

	body := func(cookie *http.Cookie) string {
		rec := x.do(http.MethodGet, "/", nil, cookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("dashboard = %d", rec.Code)
		}
		return rec.Body.String()
	}
	has := func(s, name string) bool { return strings.Contains(s, `/`+name+`"`) }

	anon := body(nil)
	if !has(anon, "pub") || has(anon, "priv") || has(anon, "mine") {
		t.Errorf("anonymous dashboard scoping wrong:\n%s", anon)
	}

	as := body(x.sessionFor(alice.ID))
	if !has(as, "pub") || !has(as, "mine") || has(as, "priv") {
		t.Errorf("ACL-user dashboard scoping wrong:\n%s", as)
	}

	admin := body(x.login(x.mintTokenFor(x.admin.ID)))
	if !has(admin, "pub") || !has(admin, "priv") || !has(admin, "mine") {
		t.Errorf("admin dashboard should list all repos:\n%s", admin)
	}
}

func TestGoldenPath(t *testing.T) {
	x := newHarness(t)
	ctx := context.Background()
	session := x.login(x.mintTokenFor(x.admin.ID))

	// Create a user to own a repo and receive grants/tokens.
	if rec := x.do(http.MethodPost, "/users", url.Values{"handle": {"alice"}}, session); rec.Code != http.StatusOK {
		t.Fatalf("create user: %d (%s)", rec.Code, rec.Body)
	}
	if _, err := x.md.GetUser(ctx, "alice"); err != nil {
		t.Fatalf("alice not created: %v", err)
	}

	// Create a repo (POST to the dashboard) — no owner field; the creating admin
	// gets admin on it.
	rec := x.do(http.MethodPost, "/",
		url.Values{"name": {"app"}, "default_branch": {"main"}}, session)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "created app") {
		t.Fatalf("create repo: %d (%s)", rec.Code, rec.Body)
	}
	repo, err := x.md.GetRepo(ctx, "app")
	if err != nil {
		t.Fatalf("repo not created: %v", err)
	}

	// Set default branch via the repo's settings page.
	rec = x.do(http.MethodPost, "/app/settings",
		url.Values{"default_branch": {"trunk"}}, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("set default branch: %d (%s)", rec.Code, rec.Body)
	}
	if got, _ := x.md.GetRepo(ctx, "app"); got.DefaultBranch != "trunk" {
		t.Errorf("default branch = %q, want trunk", got.DefaultBranch)
	}

	// Toggle visibility to public via settings.
	rec = x.do(http.MethodPost, "/app/settings", url.Values{"visibility": {"public"}}, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("set visibility: %d (%s)", rec.Code, rec.Body)
	}
	if got, _ := x.md.GetRepo(ctx, "app"); got.Visibility != meta.VisibilityPublic {
		t.Errorf("visibility = %q, want public", got.Visibility)
	}

	// Mint a token for alice — the raw token is shown exactly once in the body.
	rec = x.do(http.MethodPost, "/tokens", url.Values{"user": {"alice"}, "label": {"laptop"}}, session)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "gmt_") {
		t.Fatalf("mint token: %d (%s)", rec.Code, rec.Body)
	}
	alice, _ := x.md.GetUser(ctx, "alice")
	toks, _ := x.md.ListTokens(ctx, alice.ID)
	if len(toks) != 1 {
		t.Fatalf("alice tokens = %d, want 1", len(toks))
	}

	// Grant write (collaborator) to alice on her repo via the access page.
	rec = x.do(http.MethodPost, "/app/access",
		url.Values{"handle": {"alice"}, "perm": {"write"}}, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("grant acl: %d (%s)", rec.Code, rec.Body)
	}
	if perm, err := x.md.GetACL(ctx, repo.ID, alice.ID); err != nil || perm != meta.PermWrite {
		t.Errorf("GetACL = %q, %v; want write", perm, err)
	}

	// Revoke it again.
	rec = x.do(http.MethodPost, "/app/access/revoke",
		url.Values{"user_id": {itoa(alice.ID)}}, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke acl: %d (%s)", rec.Code, rec.Body)
	}
	if _, err := x.md.GetACL(ctx, repo.ID, alice.ID); err == nil {
		t.Error("acl still present after revoke")
	}
}

// TestCreateRepoGrantsCreatorAdmin: creating a repo through the UI grants the
// creating admin admin on it, so it is immediately usable (clone/push) without a
// separate ACL step — the gap that left a freshly created repo 403-ing every push.
func TestCreateRepoGrantsCreatorAdmin(t *testing.T) {
	x := newHarness(t)
	ctx := context.Background()
	session := x.login(x.mintTokenFor(x.admin.ID))

	if rec := x.do(http.MethodPost, "/",
		url.Values{"name": {"proj"}, "default_branch": {"main"}}, session); rec.Code != http.StatusOK {
		t.Fatalf("create repo: %d (%s)", rec.Code, rec.Body)
	}

	repo, err := x.md.GetRepo(ctx, "proj")
	if err != nil {
		t.Fatalf("repo not created: %v", err)
	}
	if perm, err := x.md.GetACL(ctx, repo.ID, x.admin.ID); err != nil || perm != meta.PermAdmin {
		t.Errorf("creator ACL after create = %q, %v; want admin", perm, err)
	}
}

func TestUnauthenticatedDenied(t *testing.T) {
	x := newHarness(t)

	// GET a management page without a session → redirect to login.
	rec := x.do(http.MethodGet, "/users", nil, nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Errorf("GET /users unauth = %d loc %q, want 303 /login", rec.Code, rec.Header().Get("Location"))
	}

	// POST without a session → 401 (not a browser navigation).
	rec = x.do(http.MethodPost, "/", url.Values{"name": {"x"}}, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("POST / (create) unauth = %d, want 401", rec.Code)
	}
}

func TestNonAdminDenied(t *testing.T) {
	x := newHarness(t)
	ctx := context.Background()

	// A regular user with a valid token CAN sign in (to browse their repos)...
	bob, _ := x.md.CreateUser(ctx, "bob")
	bobToken := x.mintTokenFor(bob.ID)
	if rec := x.do(http.MethodPost, "/login", url.Values{"token": {bobToken}}, nil); rec.Code != http.StatusSeeOther {
		t.Errorf("non-admin login = %d, want 303 (allowed)", rec.Code)
	}

	// ...but a non-admin session cannot reach a management page (403), covering
	// demotion mid-session too — the guard re-checks is_admin every request.
	forged := x.sessionFor(bob.ID)
	if rec := x.do(http.MethodGet, "/users", nil, forged); rec.Code != http.StatusForbidden {
		t.Errorf("non-admin session on /users = %d, want 403", rec.Code)
	}
}

func TestCreateRepoRejectsReservedName(t *testing.T) {
	x := newHarness(t)
	session := x.login(x.mintTokenFor(x.admin.ID))

	// A reserved repo name (a global route) is refused by meta.CreateRepo.
	rec := x.do(http.MethodPost, "/", url.Values{"name": {"login"}}, session)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "reserved") {
		t.Errorf("reserved name: %d (%s)", rec.Code, rec.Body)
	}
	if _, err := x.md.GetRepo(context.Background(), "login"); err == nil {
		t.Error("repo created despite a reserved name")
	}

	// A structurally invalid name is refused before it reaches CreateRepo.
	rec = x.do(http.MethodPost, "/", url.Values{"name": {".hidden"}}, session)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "alphanumeric") {
		t.Errorf("invalid name: %d (%s)", rec.Code, rec.Body)
	}
	if _, err := x.md.GetRepo(context.Background(), ".hidden"); err == nil {
		t.Error("repo created despite an invalid name")
	}
}

// TestSettingsVisibilityAffectsAnonBrowse: flipping a repo public via the
// settings page opens it to anonymous browsing; flipping it back closes it.
func TestSettingsVisibilityAffectsAnonBrowse(t *testing.T) {
	x := newHarness(t)
	x.seedBrowseRepo("app", "main") // private by default
	session := x.login(x.mintTokenFor(x.admin.ID))

	const target = "/app/tree/main"
	if rec := x.do(http.MethodGet, target, nil, nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("anon browse private = %d, want 303 (login)", rec.Code)
	}

	if rec := x.do(http.MethodPost, "/app/settings", url.Values{"visibility": {"public"}}, session); rec.Code != http.StatusOK {
		t.Fatalf("set public = %d (%s)", rec.Code, rec.Body)
	}
	if rec := x.do(http.MethodGet, target, nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("anon browse public = %d, want 200", rec.Code)
	}

	if rec := x.do(http.MethodPost, "/app/settings", url.Values{"visibility": {"private"}}, session); rec.Code != http.StatusOK {
		t.Fatalf("set private = %d (%s)", rec.Code, rec.Body)
	}
	if rec := x.do(http.MethodGet, target, nil, nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("anon browse re-privatized = %d, want 303", rec.Code)
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
