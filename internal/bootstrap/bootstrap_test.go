package bootstrap

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/atmin/gitmote/internal/auth"
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

func TestRunCreatesUsableInstance(t *testing.T) {
	ctx := context.Background()
	m := openMeta(t)

	res, err := Run(ctx, m, Options{AdminHandle: "atmin", RepoName: "atmin/gitmote"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.AlreadyBootstrapped {
		t.Fatal("fresh instance reported AlreadyBootstrapped")
	}
	if res.Admin == nil || !res.Admin.IsAdmin {
		t.Fatalf("admin = %+v, want a global admin", res.Admin)
	}
	if res.Repo == nil || res.Repo.Name != "atmin/gitmote" {
		t.Fatalf("repo = %+v, want atmin/gitmote", res.Repo)
	}
	if res.RawToken == "" {
		t.Fatal("no token returned")
	}

	// The printed token authenticates as the admin and is authorized to write
	// the initial repo (the admin holds a repo-admin ACL).
	guard := auth.NewGuard(m)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+res.RawToken)
	user, err := guard.Authorize(req, "atmin/gitmote", meta.PermWrite)
	if err != nil {
		t.Fatalf("Authorize with bootstrap token: %v", err)
	}
	if user.ID != res.Admin.ID {
		t.Errorf("authorized user = %d, want admin %d", user.ID, res.Admin.ID)
	}
}

func TestRunIsIdempotentAndRefusesToClobber(t *testing.T) {
	ctx := context.Background()
	m := openMeta(t)

	first, err := Run(ctx, m, Options{AdminHandle: "atmin", RepoName: "atmin/gitmote"})
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}

	// A second run must not create a new admin or mint a new token.
	second, err := Run(ctx, m, Options{AdminHandle: "someone-else", RepoName: "other/repo"})
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if !second.AlreadyBootstrapped {
		t.Error("second Run did not report AlreadyBootstrapped")
	}
	if second.RawToken != "" {
		t.Error("second Run minted a token")
	}

	// The original admin still authenticates; the would-be second admin was
	// never created.
	guard := auth.NewGuard(m)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+first.RawToken)
	if _, err := guard.Authorize(req, "atmin/gitmote", meta.PermAdmin); err != nil {
		t.Errorf("original admin no longer authorized: %v", err)
	}
	if _, err := m.GetUser(ctx, "someone-else"); err == nil {
		t.Error("second Run created a user despite an existing admin")
	}
}

func TestRunValidatesOptions(t *testing.T) {
	ctx := context.Background()
	m := openMeta(t)

	if _, err := Run(ctx, m, Options{RepoName: "a/b"}); err == nil {
		t.Error("Run without a handle succeeded, want error")
	}
	if _, err := Run(ctx, m, Options{AdminHandle: "atmin"}); err == nil {
		t.Error("Run without a repo succeeded, want error")
	}
}
