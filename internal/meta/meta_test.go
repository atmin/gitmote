package meta

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// open returns a fresh, local-only Metadata at a temp path (no replication).
func open(t *testing.T) *Metadata {
	t.Helper()
	return openAt(t, filepath.Join(t.TempDir(), "meta.sqlite3"))
}

func openAt(t *testing.T, path string) *Metadata {
	t.Helper()
	m, err := Open(context.Background(), Config{LocalPath: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// seedRepo inserts a repo and returns it; a helper for the ref/ACL tests.
func seedRepo(t *testing.T, m *Metadata, name string) *Repo {
	t.Helper()
	r, err := m.CreateRepo(context.Background(), name, "")
	if err != nil {
		t.Fatalf("CreateRepo(%q): %v", name, err)
	}
	return r
}

func TestMigrationsIdempotentAndFreshSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "meta.sqlite3")

	// Fresh DB: schema is usable — a round-trip proves the tables exist.
	m := openAt(t, path)
	r, err := m.CreateRepo(ctx, "dotfiles", "")
	if err != nil {
		t.Fatalf("CreateRepo on fresh schema: %v", err)
	}
	if r.DefaultBranch != "main" {
		t.Errorf("default branch = %q, want main", r.DefaultBranch)
	}
	if r.Visibility != VisibilityPrivate {
		t.Errorf("visibility = %q, want private", r.Visibility)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Re-open the same file: migrations are a no-op and data survives.
	m2 := openAt(t, path)
	got, err := m2.GetRepo(ctx, "dotfiles")
	if err != nil {
		t.Fatalf("GetRepo after re-open: %v", err)
	}
	if got.ID != r.ID {
		t.Errorf("repo id after re-open = %d, want %d", got.ID, r.ID)
	}
}

func TestScopedTokenColumnsMigrateAndPersist(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "meta.sqlite3")

	// A DB whose tokens table predates the scoped columns: create it by hand
	// without them, plus a legacy row, then let Open's guarded migration add them.
	m := openAt(t, path)
	u, err := m.CreateUser(ctx, "atmin")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, err := m.CreateToken(ctx, u.ID, "legacy-sel", "legacy-ver", "old"); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if _, err := m.db.ExecContext(ctx, `ALTER TABLE tokens DROP COLUMN expires_at`); err != nil {
		t.Fatalf("drop expires_at: %v", err)
	}
	if _, err := m.db.ExecContext(ctx, `ALTER TABLE tokens DROP COLUMN repo_scope`); err != nil {
		t.Fatalf("drop repo_scope: %v", err)
	}
	if _, err := m.db.ExecContext(ctx, `ALTER TABLE tokens DROP COLUMN read_only`); err != nil {
		t.Fatalf("drop read_only: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Re-open: migrate adds the columns. Open again: idempotent (no error).
	m2 := openAt(t, path)
	// The legacy row reads back with NULL/0 defaults.
	ta, err := m2.TokenBySelector(ctx, "legacy-sel")
	if err != nil {
		t.Fatalf("TokenBySelector(legacy): %v", err)
	}
	if ta.ExpiresAt != nil || ta.RepoScope != nil || ta.ReadOnly {
		t.Errorf("legacy token = %+v, want no expiry/scope, not read-only", ta)
	}
	if err := m2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	m3 := openAt(t, path) // second migrate is a no-op
	r := seedRepo(t, m3, "repo")
	exp := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	if _, err := m3.CreateScopedToken(ctx, u.ID, "sel2", "ver2", "scoped", &r.ID, true, exp); err != nil {
		t.Fatalf("CreateScopedToken: %v", err)
	}
	ta2, err := m3.TokenBySelector(ctx, "sel2")
	if err != nil {
		t.Fatalf("TokenBySelector(sel2): %v", err)
	}
	if ta2.RepoScope == nil || *ta2.RepoScope != r.ID || !ta2.ReadOnly ||
		ta2.ExpiresAt == nil || !ta2.ExpiresAt.Equal(exp) {
		t.Errorf("scoped token = %+v, want scope %d, read-only, expiry %v", ta2, r.ID, exp)
	}
}

func TestRepoCRUD(t *testing.T) {
	ctx := context.Background()
	m := open(t)

	if _, err := m.GetRepo(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetRepo(missing) = %v, want ErrNotFound", err)
	}

	seedRepo(t, m, "one")
	b := seedRepo(t, m, "two")
	if b.DefaultBranch != "main" {
		t.Errorf("default branch = %q, want main", b.DefaultBranch)
	}

	repos, err := m.ListRepos(ctx)
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if len(repos) != 2 || repos[0].Name != "one" || repos[1].Name != "two" {
		t.Errorf("ListRepos = %+v, want [one two]", repos)
	}

	// name is UNIQUE.
	if _, err := m.CreateRepo(ctx, "one", ""); err == nil {
		t.Error("CreateRepo(duplicate name) succeeded, want error")
	}
}

// TestRepoNameValidation covers the flat single-segment namespace rules: a
// trailing ".git" is stripped, reserved globals and structural forms are
// rejected with a clear error, and a ".git" clone URL resolves via GetRepo.
func TestRepoNameValidation(t *testing.T) {
	ctx := context.Background()

	// A trailing ".git" is stripped at creation and tolerated at lookup.
	m := open(t)
	r, err := m.CreateRepo(ctx, "gitmote.git", "")
	if err != nil {
		t.Fatalf("CreateRepo(gitmote.git): %v", err)
	}
	if r.Name != "gitmote" {
		t.Errorf("normalized name = %q, want gitmote", r.Name)
	}
	if got, err := m.GetRepo(ctx, "gitmote.git"); err != nil || got.ID != r.ID {
		t.Errorf("GetRepo(gitmote.git) = (%v, %v), want the same repo", got, err)
	}

	// Rejected names: reserved globals, a leading dot, the bare dash, a slash, empty.
	for _, name := range []string{"login", "api", "internal", ".hidden", "-", "a/b", ""} {
		if _, err := m.CreateRepo(ctx, name, ""); err == nil {
			t.Errorf("CreateRepo(%q) succeeded, want a validation error", name)
		}
	}
}

// TestSetVisibility flips a repo public and back, and rejects an invalid value.
func TestSetVisibility(t *testing.T) {
	ctx := context.Background()
	m := open(t)
	r := seedRepo(t, m, "repo")
	if r.Public() {
		t.Fatal("new repo should default to private")
	}

	if err := m.SetVisibility(ctx, r.ID, VisibilityPublic); err != nil {
		t.Fatalf("SetVisibility(public): %v", err)
	}
	got, _ := m.GetRepo(ctx, "repo")
	if !got.Public() {
		t.Errorf("visibility = %q, want public", got.Visibility)
	}

	// The CHECK constraint rejects an unknown value.
	if err := m.SetVisibility(ctx, r.ID, "secret"); err == nil {
		t.Error("SetVisibility(secret) succeeded, want a CHECK violation")
	}
	// Unknown repo id.
	if err := m.SetVisibility(ctx, 9999, VisibilityPublic); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetVisibility(unknown id) = %v, want ErrNotFound", err)
	}
}

// TestCanRead covers the visibility-aware repo-read shared by git and browse:
// a public repo is anyone-readable, a private repo needs an ACL.
func TestCanRead(t *testing.T) {
	ctx := context.Background()
	m := open(t)
	priv := seedRepo(t, m, "priv")
	pub := seedRepo(t, m, "pub")
	if err := m.SetVisibility(ctx, pub.ID, VisibilityPublic); err != nil {
		t.Fatalf("SetVisibility: %v", err)
	}
	pub, _ = m.GetRepo(ctx, "pub")

	u, _ := m.CreateUser(ctx, "spectator")

	cases := []struct {
		name   string
		repo   *Repo
		userID int64
		want   bool
	}{
		{"public anonymous", pub, 0, true},
		{"public user", pub, u.ID, true},
		{"private anonymous", priv, 0, false},
		{"private no ACL", priv, u.ID, false},
	}
	for _, c := range cases {
		if got, err := m.CanRead(ctx, c.repo, c.userID); err != nil || got != c.want {
			t.Errorf("%s: CanRead = (%v, %v), want %v", c.name, got, err, c.want)
		}
	}

	// A read ACL opens the private repo.
	if err := m.SetACL(ctx, priv.ID, u.ID, PermRead); err != nil {
		t.Fatalf("SetACL: %v", err)
	}
	if got, err := m.CanRead(ctx, priv, u.ID); err != nil || !got {
		t.Errorf("private with read ACL: CanRead = (%v, %v), want true", got, err)
	}
}

func TestListReposForViewer(t *testing.T) {
	ctx := context.Background()
	m := open(t)
	pub := seedRepo(t, m, "pub")
	if err := m.SetVisibility(ctx, pub.ID, VisibilityPublic); err != nil {
		t.Fatalf("SetVisibility: %v", err)
	}
	seedRepo(t, m, "priv") // admin-only, no ACL
	mine := seedRepo(t, m, "mine")
	u, _ := m.CreateUser(ctx, "alice")
	if err := m.SetACL(ctx, mine.ID, u.ID, PermRead); err != nil {
		t.Fatalf("SetACL: %v", err)
	}

	names := func(userID int64) []string {
		repos, err := m.ListReposForViewer(ctx, userID)
		if err != nil {
			t.Fatalf("ListReposForViewer: %v", err)
		}
		var out []string
		for _, r := range repos {
			out = append(out, r.Name)
		}
		return out
	}

	// Anonymous (userID 0) sees only public repos.
	if got := names(0); len(got) != 1 || got[0] != "pub" {
		t.Errorf("anonymous viewer repos = %v, want [pub]", got)
	}
	// alice sees public + the one she has an ACL on (ordered by name).
	if got := names(u.ID); len(got) != 2 || got[0] != "mine" || got[1] != "pub" {
		t.Errorf("alice viewer repos = %v, want [mine pub]", got)
	}
}

func TestTokenStorage(t *testing.T) {
	ctx := context.Background()
	m := open(t)

	u, err := m.CreateUser(ctx, "atmin")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	const (
		selector = "0123456789abcdef"
		verifier = "deadbeef" // stands in for SHA-256(secret); opaque to meta
	)
	tok, err := m.CreateToken(ctx, u.ID, selector, verifier, "laptop")
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	// Lookup by selector returns the verifier + owner, and does not touch
	// last_used.
	ta, err := m.TokenBySelector(ctx, selector)
	if err != nil {
		t.Fatalf("TokenBySelector: %v", err)
	}
	if ta.TokenID != tok.ID || ta.Verifier != verifier || ta.User.ID != u.ID || ta.User.Handle != "atmin" {
		t.Errorf("TokenBySelector = %+v, want token %d verifier %q user %d atmin", ta, tok.ID, verifier, u.ID)
	}

	before, err := m.ListTokens(ctx, u.ID)
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	if len(before) != 1 || before[0].Label != "laptop" || before[0].LastUsed != nil {
		t.Errorf("ListTokens = %+v, want one 'laptop' token with last_used unset", before)
	}

	// TouchToken stamps last_used.
	if err := m.TouchToken(ctx, tok.ID); err != nil {
		t.Fatalf("TouchToken: %v", err)
	}
	after, _ := m.ListTokens(ctx, u.ID)
	if after[0].LastUsed == nil {
		t.Error("last_used still unset after TouchToken")
	}

	// An unknown selector is ErrNotFound.
	if _, err := m.TokenBySelector(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("TokenBySelector(unknown) = %v, want ErrNotFound", err)
	}

	// selector is UNIQUE.
	if _, err := m.CreateToken(ctx, u.ID, selector, "other", "dup"); err == nil {
		t.Error("CreateToken(duplicate selector) succeeded, want error")
	}
}

func TestACLLookup(t *testing.T) {
	ctx := context.Background()
	m := open(t)

	r := seedRepo(t, m, "repo")
	u, err := m.CreateUser(ctx, "atmin")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	if _, err := m.GetACL(ctx, r.ID, u.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetACL(unset) = %v, want ErrNotFound", err)
	}

	if err := m.SetACL(ctx, r.ID, u.ID, PermRead); err != nil {
		t.Fatalf("SetACL(read): %v", err)
	}
	if p, _ := m.GetACL(ctx, r.ID, u.ID); p != PermRead {
		t.Errorf("GetACL = %q, want read", p)
	}

	// Upsert replaces the level.
	if err := m.SetACL(ctx, r.ID, u.ID, PermAdmin); err != nil {
		t.Fatalf("SetACL(admin): %v", err)
	}
	if p, _ := m.GetACL(ctx, r.ID, u.ID); p != PermAdmin {
		t.Errorf("GetACL after upsert = %q, want admin", p)
	}

	// The CHECK constraint rejects a bogus level.
	if err := m.SetACL(ctx, r.ID, u.ID, Perm("root")); err == nil {
		t.Error("SetACL(bogus perm) succeeded, want CHECK violation")
	}
}
