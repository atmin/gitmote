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

// verifyToken resolves raw to its owner — the token-only check, no repo needed.
func verifyToken(t *testing.T, m *meta.Metadata, raw string) (*meta.User, error) {
	t.Helper()
	return auth.NewGuard(m).VerifyToken(context.Background(), raw)
}

func TestRunCreatesUsableInstanceWithRepo(t *testing.T) {
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

	// The printed token authenticates as the admin and is authorized to write the
	// initial repo (the admin holds a repo-admin ACL).
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+res.RawToken)
	user, err := auth.NewGuard(m).Authorize(req, "atmin/gitmote", meta.PermWrite)
	if err != nil {
		t.Fatalf("Authorize with bootstrap token: %v", err)
	}
	if user.ID != res.Admin.ID {
		t.Errorf("authorized user = %d, want admin %d", user.ID, res.Admin.ID)
	}
}

func TestRunDefaultsHandleAndSkipsRepo(t *testing.T) {
	ctx := context.Background()
	m := openMeta(t)

	// No handle and no repo: the admin defaults to "admin" and no repo is made.
	res, err := Run(ctx, m, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Admin == nil || res.Admin.Handle != DefaultAdminHandle {
		t.Fatalf("admin = %+v, want default handle %q", res.Admin, DefaultAdminHandle)
	}
	if res.Repo != nil {
		t.Errorf("repo = %+v, want none created", res.Repo)
	}
	if res.RawToken == "" {
		t.Fatal("no token returned")
	}
	// The token authenticates as the (global-admin) user with no repo present.
	if _, err := verifyToken(t, m, res.RawToken); err != nil {
		t.Fatalf("VerifyToken with bootstrap token: %v", err)
	}
}

func TestRunIsIdempotentAndRefusesToClobber(t *testing.T) {
	ctx := context.Background()
	m := openMeta(t)

	first, err := Run(ctx, m, Options{AdminHandle: "atmin"})
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}

	// A second run must not create a new admin or mint a new token.
	second, err := Run(ctx, m, Options{AdminHandle: "someone-else"})
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if !second.AlreadyBootstrapped {
		t.Error("second Run did not report AlreadyBootstrapped")
	}
	if second.RawToken != "" {
		t.Error("second Run minted a token")
	}

	// The original admin still authenticates; the would-be second admin was never
	// created.
	if _, err := verifyToken(t, m, first.RawToken); err != nil {
		t.Errorf("original admin no longer authorized: %v", err)
	}
	if _, err := m.GetUser(ctx, "someone-else"); err == nil {
		t.Error("second Run created a user despite an existing admin")
	}
}

func TestReissueMintsFreshTokenForExistingAdmin(t *testing.T) {
	ctx := context.Background()
	m := openMeta(t)

	first, err := Run(ctx, m, Options{AdminHandle: "atmin"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	res, err := Reissue(ctx, m, Options{AdminHandle: "atmin"})
	if err != nil {
		t.Fatalf("Reissue: %v", err)
	}
	if res.RawToken == "" || res.RawToken == first.RawToken {
		t.Fatalf("reissued token = %q, want a fresh non-empty token", res.RawToken)
	}
	if res.Admin.ID != first.Admin.ID {
		t.Errorf("reissued for user %d, want the existing admin %d", res.Admin.ID, first.Admin.ID)
	}

	// Both the old and the freshly-reissued token authenticate as the admin (the
	// old one isn't revoked — reissue only adds a new token).
	if _, err := verifyToken(t, m, res.RawToken); err != nil {
		t.Errorf("reissued token not authorized: %v", err)
	}
	if _, err := verifyToken(t, m, first.RawToken); err != nil {
		t.Errorf("original token no longer authorized after reissue: %v", err)
	}
}

func TestReissueRequiresAnExistingAdmin(t *testing.T) {
	ctx := context.Background()
	m := openMeta(t)

	// No admin yet: reissue has nothing to reissue for.
	if _, err := Reissue(ctx, m, Options{}); err == nil {
		t.Error("Reissue on an empty instance succeeded, want an error")
	}

	// A non-admin user must not be reissued for either.
	if _, err := m.CreateUser(ctx, "plain"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, err := Reissue(ctx, m, Options{AdminHandle: "plain"}); err == nil {
		t.Error("Reissue for a non-admin succeeded, want an error")
	}
}
