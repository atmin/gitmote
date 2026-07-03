package webui

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/atmin/gitmote/internal/meta"
)

// gitTest runs a git command hermetically, failing the test on a nonzero exit.
func gitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// seedBrowseRepo builds a real repo (a README.go, a subdir, a binary blob, a
// tag), loads its objects into the harness store under name, and records the
// repo + branch ref in meta. It returns (headSHA, firstSHA).
func (x *harness) seedBrowseRepo(name, branch string) (head, first string) {
	x.t.Helper()
	ctx := context.Background()

	src := x.t.TempDir()
	gitTest(x.t, src, "init", "-b", branch, ".")
	write(x.t, src, "hello.go", "package main\n\nfunc main() {}\n")
	write(x.t, src, "sub/note.txt", "nested\n")
	write(x.t, src, "bin.dat", "a\x00b\x00c")
	gitTest(x.t, src, "add", "-A")
	gitTest(x.t, src, "commit", "-m", "first")
	first = gitTest(x.t, src, "rev-parse", "HEAD")

	write(x.t, src, "hello.go", "package main\n\nfunc main() { println(1) }\n")
	gitTest(x.t, src, "commit", "-am", "second")
	head = gitTest(x.t, src, "rev-parse", "HEAD")
	gitTest(x.t, src, "tag", "v1")

	// Mirror .git/objects/… into the store under {name}/objects/….
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
		return x.store.Put(ctx, name+"/objects/"+filepath.ToSlash(rel), bytes.NewReader(data))
	})
	if err != nil {
		x.t.Fatalf("seed objects: %v", err)
	}

	r, err := x.md.CreateRepo(ctx, name, branch)
	if err != nil {
		x.t.Fatalf("CreateRepo: %v", err)
	}
	if err := x.md.CASRef(ctx, r.ID, "refs/heads/"+branch, meta.ZeroSHA, head); err != nil {
		x.t.Fatalf("CASRef branch: %v", err)
	}
	if err := x.md.CASRef(ctx, r.ID, "refs/tags/v1", meta.ZeroSHA, head); err != nil {
		x.t.Fatalf("CASRef tag: %v", err)
	}
	return head, first
}

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBrowseRequiresAdmin(t *testing.T) {
	x := newHarness(t)
	x.seedBrowseRepo("alice/app", "main")

	// No session cookie: a GET redirects to login rather than serving content.
	rec := x.do(http.MethodGet, "/browse/alice/app/-/tree/", nil, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("unauth browse = %d, want 303", rec.Code)
	}
}

func TestBrowseTreeAndBlob(t *testing.T) {
	x := newHarness(t)
	x.seedBrowseRepo("alice/app", "main")
	session := x.login(x.mintTokenFor(x.admin.ID))

	// Root tree lists the top-level entries.
	rec := x.do(http.MethodGet, "/browse/alice/app/-/tree/", nil, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("tree = %d (%s)", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{"hello.go", "sub", "bin.dat"} {
		if !strings.Contains(body, want) {
			t.Fatalf("tree body missing %q:\n%s", want, body)
		}
	}

	// Subdirectory tree.
	rec = x.do(http.MethodGet, "/browse/alice/app/-/tree/sub?ref=main", nil, session)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "note.txt") {
		t.Fatalf("sub tree = %d (%s)", rec.Code, rec.Body)
	}

	// Text blob renders inside a <pre>.
	rec = x.do(http.MethodGet, "/browse/alice/app/-/blob/hello.go?ref=main", nil, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("blob = %d (%s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "package main") {
		t.Fatalf("blob body missing content:\n%s", rec.Body)
	}

	// Binary blob shows the download affordance, not garbage.
	rec = x.do(http.MethodGet, "/browse/alice/app/-/blob/bin.dat?ref=main", nil, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("binary blob = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Binary file") {
		t.Fatalf("binary blob missing download affordance:\n%s", rec.Body)
	}
}

func TestBrowseRawDownload(t *testing.T) {
	x := newHarness(t)
	x.seedBrowseRepo("alice/app", "main")
	session := x.login(x.mintTokenFor(x.admin.ID))

	rec := x.do(http.MethodGet, "/browse/alice/app/-/raw/sub/note.txt?ref=main", nil, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("raw = %d (%s)", rec.Code, rec.Body)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") || !strings.Contains(cd, "note.txt") {
		t.Fatalf("raw Content-Disposition = %q", cd)
	}
	if rec.Body.String() != "nested\n" {
		t.Fatalf("raw body = %q", rec.Body.String())
	}
}

func TestBrowseRefSwitcher(t *testing.T) {
	x := newHarness(t)
	x.seedBrowseRepo("alice/app", "main")
	session := x.login(x.mintTokenFor(x.admin.ID))

	rec := x.do(http.MethodGet, "/browse/alice/app/-/tree/", nil, session)
	body := rec.Body.String()
	// The switcher lists both the branch and the tag from meta.ListRefs.
	if !strings.Contains(body, `value="main"`) || !strings.Contains(body, `value="v1"`) {
		t.Fatalf("ref switcher missing branch/tag:\n%s", body)
	}
}

func TestBrowseCommitsAndCommit(t *testing.T) {
	x := newHarness(t)
	head, _ := x.seedBrowseRepo("alice/app", "main")
	session := x.login(x.mintTokenFor(x.admin.ID))

	rec := x.do(http.MethodGet, "/browse/alice/app/-/commits?ref=main", nil, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("commits = %d (%s)", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "first") || !strings.Contains(body, "second") {
		t.Fatalf("commits body missing subjects:\n%s", body)
	}

	rec = x.do(http.MethodGet, "/browse/alice/app/-/commit/"+head, nil, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("commit = %d (%s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), head) {
		t.Fatalf("commit body missing sha:\n%s", rec.Body)
	}
}

func TestBrowseNotFoundAndTraversal(t *testing.T) {
	x := newHarness(t)
	x.seedBrowseRepo("alice/app", "main")
	session := x.login(x.mintTokenFor(x.admin.ID))

	cases := []struct {
		name, target string
	}{
		{"unknown repo", "/browse/bob/ghost/-/tree/?ref=main"},
		{"unknown ref", "/browse/alice/app/-/tree/?ref=nope"},
		// The path arrives as an uncleaned query param, so it reaches the
		// reader's safePath guard rather than being normalized by the mux.
		{"path traversal", "/browse/alice/app/-/commits?ref=main&path=../etc/passwd"},
		{"missing blob", "/browse/alice/app/-/blob/nope.txt?ref=main"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := x.do(http.MethodGet, tc.target, nil, session)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s = %d, want 404 (%s)", tc.name, rec.Code, rec.Body)
			}
		})
	}
}
