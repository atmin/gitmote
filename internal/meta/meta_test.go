package meta

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
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
	r, err := m.CreateRepo(ctx, "atmin/dotfiles", "")
	if err != nil {
		t.Fatalf("CreateRepo on fresh schema: %v", err)
	}
	if r.DefaultBranch != "main" {
		t.Errorf("default branch = %q, want main", r.DefaultBranch)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Re-open the same file: migrations are a no-op and data survives.
	m2 := openAt(t, path)
	got, err := m2.GetRepo(ctx, "atmin/dotfiles")
	if err != nil {
		t.Fatalf("GetRepo after re-open: %v", err)
	}
	if got.ID != r.ID {
		t.Errorf("repo id after re-open = %d, want %d", got.ID, r.ID)
	}
}

func TestRepoCRUD(t *testing.T) {
	ctx := context.Background()
	m := open(t)

	if _, err := m.GetRepo(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetRepo(missing) = %v, want ErrNotFound", err)
	}

	seedRepo(t, m, "a/one")
	b := seedRepo(t, m, "b/two")
	if b.DefaultBranch != "main" {
		t.Errorf("default branch = %q, want main", b.DefaultBranch)
	}

	repos, err := m.ListRepos(ctx)
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if len(repos) != 2 || repos[0].Name != "a/one" || repos[1].Name != "b/two" {
		t.Errorf("ListRepos = %+v, want [a/one b/two]", repos)
	}

	// name is UNIQUE.
	if _, err := m.CreateRepo(ctx, "a/one", ""); err == nil {
		t.Error("CreateRepo(duplicate name) succeeded, want error")
	}
}

func TestUserAndTokenAuth(t *testing.T) {
	ctx := context.Background()
	m := open(t)

	u, err := m.CreateUser(ctx, "atmin")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	const raw = "pat_supersecret_value"
	tok, err := m.CreateToken(ctx, u.ID, raw, "laptop")
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	// The raw token is never stored.
	var stored string
	if err := m.db.QueryRowContext(ctx, `SELECT hash FROM tokens WHERE id = ?`, tok.ID).Scan(&stored); err != nil {
		t.Fatalf("read hash: %v", err)
	}
	if stored == raw {
		t.Fatal("raw token stored in tokens.hash")
	}
	if stored != HashToken(raw) {
		t.Errorf("stored hash = %q, want %q", stored, HashToken(raw))
	}

	// Authentication resolves the owner and stamps last_used.
	got, err := m.AuthenticateToken(ctx, raw)
	if err != nil {
		t.Fatalf("AuthenticateToken: %v", err)
	}
	if got.ID != u.ID || got.Handle != "atmin" {
		t.Errorf("AuthenticateToken = %+v, want user %d atmin", got, u.ID)
	}
	toks, err := m.ListTokens(ctx, u.ID)
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	if len(toks) != 1 || toks[0].Label != "laptop" || toks[0].LastUsed == nil {
		t.Errorf("ListTokens = %+v, want one 'laptop' token with last_used set", toks)
	}

	// A wrong token authenticates no one.
	if _, err := m.AuthenticateToken(ctx, "pat_wrong"); !errors.Is(err, ErrNotFound) {
		t.Errorf("AuthenticateToken(wrong) = %v, want ErrNotFound", err)
	}
}

func TestACLLookup(t *testing.T) {
	ctx := context.Background()
	m := open(t)

	r := seedRepo(t, m, "atmin/repo")
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
