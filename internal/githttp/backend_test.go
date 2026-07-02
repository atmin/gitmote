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

	"github.com/atmin/gitmote/internal/meta"
	"github.com/atmin/gitmote/internal/repo"
	"github.com/atmin/gitmote/internal/store"
)

// git runs a git command hermetically and returns its trimmed combined output,
// failing the test on a nonzero exit.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
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
func loadObjects(t *testing.T, s store.Store, src, repoName string) {
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
		return s.Put(context.Background(), repoName+"/objects/"+filepath.ToSlash(rel), bytes.NewReader(data))
	})
	if err != nil {
		t.Fatalf("load objects: %v", err)
	}
}

// newServer wires meta + mem store + materializer behind a read-path handler on
// an httptest server, returning the server and its backing pieces.
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
	h, err := New(repo.New(m, s, t.TempDir()), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("githttp.New: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, m, s
}

func TestCloneAndFetch(t *testing.T) {
	ctx := context.Background()
	srv, m, s := newServer(t)
	const repoName = "atmin/dotfiles"

	// Seed: a source repo with one commit → objects into the store, ref into
	// meta.
	src := t.TempDir()
	git(t, src, "init", "-b", "main", ".")
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, src, "add", "README.md")
	git(t, src, "commit", "-m", "first")
	loadObjects(t, s, src, repoName)

	head1 := git(t, src, "rev-parse", "HEAD")
	r, err := m.CreateRepo(ctx, repoName, "main")
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	if err := m.CASRef(ctx, r.ID, "refs/heads/main", meta.ZeroSHA, head1); err != nil {
		t.Fatalf("CASRef: %v", err)
	}

	// Golden path: a real clone over HTTP.
	dst := filepath.Join(t.TempDir(), "clone")
	git(t, "", "clone", srv.URL+"/"+repoName, dst)

	if got := git(t, dst, "rev-parse", "HEAD"); got != head1 {
		t.Errorf("cloned HEAD = %q, want %q", got, head1)
	}
	if data, err := os.ReadFile(filepath.Join(dst, "README.md")); err != nil || string(data) != "hello\n" {
		t.Errorf("cloned README = %q (err %v), want \"hello\\n\"", data, err)
	}
	git(t, dst, "fsck", "--strict") // fails the test on any corruption/missing object

	// Seed an update: a second commit on the server side.
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, src, "commit", "-am", "second")
	loadObjects(t, s, src, repoName)
	head2 := git(t, src, "rev-parse", "HEAD")
	if err := m.CASRef(ctx, r.ID, "refs/heads/main", head1, head2); err != nil {
		t.Fatalf("CASRef advance: %v", err)
	}

	// fetch pulls the new commit.
	git(t, dst, "fetch", "origin")
	if got := git(t, dst, "rev-parse", "origin/main"); got != head2 {
		t.Errorf("origin/main after fetch = %q, want %q", got, head2)
	}
}

func TestCloneUnknownRepoFails(t *testing.T) {
	srv, _, _ := newServer(t)
	// A clone of a repo with no metadata row must not succeed.
	dst := filepath.Join(t.TempDir(), "clone")
	cmd := exec.Command("git", "clone", srv.URL+"/no/such", dst)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("clone of unknown repo succeeded, want failure\n%s", out)
	}
}

func TestReceivePackNotServed(t *testing.T) {
	srv, _, _ := newServer(t)
	// The write service advertisement is not part of the read path (task 07).
	resp, err := http.Get(srv.URL + "/atmin/dotfiles/info/refs?service=git-receive-pack")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("receive-pack advertisement status = %d, want 404", resp.StatusCode)
	}
}

func TestParseReadPath(t *testing.T) {
	cases := []struct {
		path         string
		wantRepo     string
		wantEndpoint string
		wantOK       bool
	}{
		{"/atmin/dotfiles/info/refs", "atmin/dotfiles", "info/refs", true},
		{"/atmin/dotfiles/git-upload-pack", "atmin/dotfiles", "git-upload-pack", true},
		{"/one/info/refs", "one", "info/refs", true},
		{"/info/refs", "", "", false},           // no repo
		{"/git-upload-pack", "", "", false},     // no repo
		{"/atmin/dotfiles/HEAD", "", "", false}, // not a served endpoint
		{"/", "", "", false},
	}
	for _, c := range cases {
		repoName, endpoint, ok := parseReadPath(c.path)
		if repoName != c.wantRepo || endpoint != c.wantEndpoint || ok != c.wantOK {
			t.Errorf("parseReadPath(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.path, repoName, endpoint, ok, c.wantRepo, c.wantEndpoint, c.wantOK)
		}
	}
}
