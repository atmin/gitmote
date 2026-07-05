package githttp

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/atmin/gitmote/internal/auth"
	"github.com/atmin/gitmote/internal/meta"
	"github.com/atmin/gitmote/internal/repo"
	"github.com/atmin/gitmote/internal/store"
)

const repoName = "dotfiles"

// git runs a git command hermetically and returns its trimmed combined output,
// failing the test on a nonzero exit.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	// Disable any credential helper so a token in one URL is never cached and
	// silently reused by a later "anonymous" request to the same host:port.
	cmd := exec.CommandContext(context.Background(), "git", append([]string{"-c", "credential.helper="}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// loadObjects mirrors a source repo's loose/packed objects into the store under
// the repo's prefix — the durable objects a read hydrates from.
func loadObjects(t *testing.T, s store.Store, src, name string) {
	t.Helper()
	objRoot := filepath.Join(src, ".git", "objects")
	err := filepath.WalkDir(objRoot, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(objRoot, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return s.Put(context.Background(), name+"/objects/"+filepath.ToSlash(rel), bytes.NewReader(data))
	})
	if err != nil {
		t.Fatalf("load objects: %v", err)
	}
}

// newServer wires meta + mem store + materializer behind a guarded read-path
// handler on an httptest server.
func newServer(t *testing.T) (*httptest.Server, *meta.Metadata, store.Store) {
	t.Helper()
	m, err := meta.Open(context.Background(), meta.Config{
		LocalPath: filepath.Join(t.TempDir(), "meta.sqlite3"),
	})
	if err != nil {
		t.Fatalf("meta.Open: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })

	s := store.NewMem()
	h, err := New(Config{
		Materializer: repo.New(m, s, t.TempDir()),
		Authorizer:   auth.NewGuard(m),
		Logger:       slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("githttp.New: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, m, s
}

// seedRepo builds a one-commit source repo, loads its objects, and records the
// repo + main ref, returning (repoID, headSHA).
func seedRepo(t *testing.T, m *meta.Metadata, s store.Store, src string) (int64, string) {
	t.Helper()
	ctx := context.Background()
	git(t, src, "init", "-b", "main", ".")
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, src, "add", "README.md")
	git(t, src, "commit", "-m", "first")
	loadObjects(t, s, src, repoName)

	head := git(t, src, "rev-parse", "HEAD")
	r, err := m.CreateRepo(ctx, repoName, "main")
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	if err := m.CASRef(ctx, r.ID, "refs/heads/main", meta.ZeroSHA, head); err != nil {
		t.Fatalf("CASRef: %v", err)
	}
	return r.ID, head
}

// mintUser creates a user with a fresh token and returns the raw token. When
// perm is non-empty it also grants that permission on repoID.
func mintUser(t *testing.T, m *meta.Metadata, repoID int64, handle string, perm meta.Perm) string {
	t.Helper()
	ctx := context.Background()
	u, err := m.CreateUser(ctx, handle)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	raw, selector, verifier, err := auth.Mint()
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, err := m.CreateToken(ctx, u.ID, selector, verifier, "test"); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if perm != "" {
		if err := m.SetACL(ctx, repoID, u.ID, perm); err != nil {
			t.Fatalf("SetACL: %v", err)
		}
	}
	return raw
}

// authedURL embeds Basic credentials (git's native HTTP auth) into a base URL.
func authedURL(base, raw string) string {
	return strings.Replace(base, "://", "://git:"+raw+"@", 1)
}

func TestCloneAndFetch(t *testing.T) {
	ctx := context.Background()
	srv, m, s := newServer(t)

	src := t.TempDir()
	repoID, head1 := seedRepo(t, m, s, src)
	raw := mintUser(t, m, repoID, "atmin", meta.PermRead)

	// Golden path: a real clone over HTTP with a read token.
	dst := filepath.Join(t.TempDir(), "clone")
	git(t, "", "clone", authedURL(srv.URL, raw)+"/"+repoName, dst)

	if got := git(t, dst, "rev-parse", "HEAD"); got != head1 {
		t.Errorf("cloned HEAD = %q, want %q", got, head1)
	}
	if data, err := os.ReadFile(filepath.Join(dst, "README.md")); err != nil || string(data) != "hello\n" {
		t.Errorf("cloned README = %q (err %v), want \"hello\\n\"", data, err)
	}
	git(t, dst, "fsck", "--strict")

	// Seed a second commit server-side.
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, src, "commit", "-am", "second")
	loadObjects(t, s, src, repoName)
	head2 := git(t, src, "rev-parse", "HEAD")
	r, _ := m.GetRepo(ctx, repoName)
	if err := m.CASRef(ctx, r.ID, "refs/heads/main", head1, head2); err != nil {
		t.Fatalf("CASRef advance: %v", err)
	}

	// An incremental fetch pulls the new commit (explicit URL carries creds).
	git(t, dst, "fetch", authedURL(srv.URL, raw)+"/"+repoName, "main")
	if got := git(t, dst, "rev-parse", "FETCH_HEAD"); got != head2 {
		t.Errorf("FETCH_HEAD after fetch = %q, want %q", got, head2)
	}
}

// infoRefs GETs the upload-pack advertisement with an optional bearer token and
// returns the status code.
func infoRefs(t *testing.T, srvURL, token string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srvURL+"/"+repoName+"/info/refs?service=git-upload-pack", nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET info/refs: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized && resp.Header.Get("WWW-Authenticate") == "" {
		t.Error("401 without WWW-Authenticate challenge")
	}
	return resp.StatusCode
}

func TestReadAuthorization(t *testing.T) {
	srv, m, s := newServer(t)
	repoID, _ := seedRepo(t, m, s, t.TempDir())

	reader := mintUser(t, m, repoID, "reader", meta.PermRead)
	noACL := mintUser(t, m, repoID, "stranger", "")

	cases := []struct {
		name  string
		token string
		want  int
	}{
		{"no token", "", http.StatusUnauthorized},
		{"malformed token", "not-a-token", http.StatusUnauthorized},
		{"unknown token", "gmt_00000000000000000000000000000000.1111", http.StatusUnauthorized},
		{"valid token without ACL", noACL, http.StatusForbidden},
		{"valid read token", reader, http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := infoRefs(t, srv.URL, c.token); got != c.want {
				t.Errorf("status = %d, want %d", got, c.want)
			}
		})
	}
}

func TestWrongSecretRejected(t *testing.T) {
	srv, m, s := newServer(t)
	repoID, _ := seedRepo(t, m, s, t.TempDir())
	raw := mintUser(t, m, repoID, "reader", meta.PermRead)

	// Same selector, tampered secret: the constant-time verifier compare fails.
	selector, _, _ := strings.Cut(strings.TrimPrefix(raw, "gmt_"), ".")
	forged := "gmt_" + selector + ".deadbeefdeadbeef"
	if got := infoRefs(t, srv.URL, forged); got != http.StatusUnauthorized {
		t.Errorf("forged-secret status = %d, want 401", got)
	}
}

func TestReceivePackNotServed(t *testing.T) {
	srv, m, s := newServer(t)
	repoID, _ := seedRepo(t, m, s, t.TempDir())
	raw := mintUser(t, m, repoID, "reader", meta.PermRead)

	// The write service advertisement is not part of the read path (task 07) —
	// 404 even for an authorized reader.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/"+repoName+"/info/refs?service=git-receive-pack", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("receive-pack advertisement status = %d, want 404", resp.StatusCode)
	}
}

func TestParseGitPath(t *testing.T) {
	cases := []struct {
		path         string
		wantRepo     string
		wantEndpoint string
		wantOK       bool
	}{
		{"/dotfiles/info/refs", "dotfiles", "info/refs", true},
		{"/dotfiles/git-upload-pack", "dotfiles", "git-upload-pack", true},
		{"/dotfiles/git-receive-pack", "dotfiles", "git-receive-pack", true},
		{"/one/info/refs", "one", "info/refs", true},
		{"/info/refs", "", "", false},       // no repo
		{"/git-upload-pack", "", "", false}, // no repo
		{"/dotfiles/HEAD", "", "", false},   // not a served endpoint
		{"/", "", "", false},
	}
	for _, c := range cases {
		gotRepo, gotEndpoint, ok := parseGitPath(c.path)
		if gotRepo != c.wantRepo || gotEndpoint != c.wantEndpoint || ok != c.wantOK {
			t.Errorf("parseGitPath(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.path, gotRepo, gotEndpoint, ok, c.wantRepo, c.wantEndpoint, c.wantOK)
		}
	}
}
