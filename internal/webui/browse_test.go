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
	write(x.t, src, "README.md", "# App\n\nHello **world**.\n\n```go\nfunc main() {}\n```\n")
	write(x.t, src, "diagram.md", "# Diagram\n\n```mermaid\ngraph TD\n  A --> B\n```\n")
	write(x.t, src, "sub/note.txt", "nested\n")
	write(x.t, src, "bin.dat", "a\x00b\x00c")
	write(x.t, src, "big.go", "package main\n"+strings.Repeat("// filler line\n", 40000))
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
	// Browse is gated on repo-read; grant the harness admin an ACL so a logged-in
	// admin can browse this (private-by-default) repo.
	if err := x.md.SetACL(ctx, r.ID, x.admin.ID, meta.PermAdmin); err != nil {
		x.t.Fatalf("SetACL: %v", err)
	}
	if err := x.md.CASRef(ctx, r.ID, "refs/heads/"+branch, meta.ZeroSHA, head); err != nil {
		x.t.Fatalf("CASRef branch: %v", err)
	}
	if err := x.md.CASRef(ctx, r.ID, "refs/tags/v1", meta.ZeroSHA, head); err != nil {
		x.t.Fatalf("CASRef tag: %v", err)
	}
	return head, first
}

// TestBrowseBlobOnDirRedirects: blob on a directory 301s to the tree URL,
// canonicalizing rather than 404ing (the self-healing verb).
func TestBrowseBlobOnDirRedirects(t *testing.T) {
	x := newHarness(t)
	x.seedBrowseRepo("app", "main")
	session := x.login(x.mintTokenFor(x.admin.ID))

	rec := x.do(http.MethodGet, "/app/blob/main/sub", nil, session)
	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("blob-on-dir = %d, want 301 (%s)", rec.Code, rec.Body)
	}
	if loc := rec.Header().Get("Location"); loc != "/app/tree/main/sub" {
		t.Fatalf("blob-on-dir redirected to %q, want /app/tree/main/sub", loc)
	}
}

// TestBrowseGreedyRef covers ref-in-path resolution: a slashed branch, and a
// branch that beats a same-named tag on a tie.
func TestBrowseGreedyRef(t *testing.T) {
	x := newHarness(t)
	head, first := x.seedBrowseRepo("app", "main")
	session := x.login(x.mintTokenFor(x.admin.ID))
	ctx := context.Background()
	r, _ := x.md.GetRepo(ctx, "app")

	// A slashed branch, plus a branch and a tag sharing the name "rel" (branch at
	// head, tag at the first commit) to exercise the branch-over-tag tie-break.
	if err := x.md.CASRef(ctx, r.ID, "refs/heads/feature/x", meta.ZeroSHA, head); err != nil {
		t.Fatalf("CASRef feature/x: %v", err)
	}
	if err := x.md.CASRef(ctx, r.ID, "refs/heads/rel", meta.ZeroSHA, head); err != nil {
		t.Fatalf("CASRef branch rel: %v", err)
	}
	if err := x.md.CASRef(ctx, r.ID, "refs/tags/rel", meta.ZeroSHA, first); err != nil {
		t.Fatalf("CASRef tag rel: %v", err)
	}

	// Slashed branch: ref "feature/x", path "README.md" → the rendered README.
	rec := x.do(http.MethodGet, "/app/tree/feature/x/README.md", nil, session)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<h1") {
		t.Fatalf("slashed-branch blob = %d (%s)", rec.Code, rec.Body)
	}

	// Tie-break: "rel" resolves to the branch (head, which has println) not the
	// tag (first, which does not).
	rec = x.do(http.MethodGet, "/app/blob/rel/hello.go", nil, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("tie-break blob = %d (%s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "println") {
		t.Fatalf("tie-break resolved the tag, not the branch:\n%s", rec.Body)
	}
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

// TestBrowseAccessControl exercises the repo-read gate on browse: a private repo
// is browsable by the admin and a read-ACL spectator, refused (403) for a
// signed-in stranger, and redirects an anonymous viewer to login; flipping it
// public opens it to anyone.
func TestBrowseAccessControl(t *testing.T) {
	x := newHarness(t)
	ctx := context.Background()
	x.seedBrowseRepo("app", "main") // private by default; grants the admin an ACL
	r, _ := x.md.GetRepo(ctx, "app")

	adminSession := x.login(x.mintTokenFor(x.admin.ID))

	// A spectator: a non-admin with a read ACL.
	spec, _ := x.md.CreateUser(ctx, "spectator")
	if err := x.md.SetACL(ctx, r.ID, spec.ID, meta.PermRead); err != nil {
		t.Fatalf("SetACL: %v", err)
	}
	specSession := x.sessionFor(spec.ID)

	// A stranger: a signed-in user with no ACL.
	stranger, _ := x.md.CreateUser(ctx, "stranger")
	strangerSession := x.sessionFor(stranger.ID)

	const target = "/app/tree/main"

	if rec := x.do(http.MethodGet, target, nil, adminSession); rec.Code != http.StatusOK {
		t.Errorf("admin browse private = %d, want 200", rec.Code)
	}
	if rec := x.do(http.MethodGet, target, nil, specSession); rec.Code != http.StatusOK {
		t.Errorf("spectator browse private = %d, want 200", rec.Code)
	}
	if rec := x.do(http.MethodGet, target, nil, strangerSession); rec.Code != http.StatusForbidden {
		t.Errorf("stranger browse private = %d, want 403", rec.Code)
	}
	if rec := x.do(http.MethodGet, target, nil, nil); rec.Code != http.StatusSeeOther {
		t.Errorf("anonymous browse private = %d, want 303 (login)", rec.Code)
	}

	// Make it public: an anonymous viewer may now browse.
	if err := x.md.SetVisibility(ctx, r.ID, meta.VisibilityPublic); err != nil {
		t.Fatalf("SetVisibility: %v", err)
	}
	if rec := x.do(http.MethodGet, target, nil, nil); rec.Code != http.StatusOK {
		t.Errorf("anonymous browse public = %d, want 200", rec.Code)
	}
}

func TestBrowseTreeAndBlob(t *testing.T) {
	x := newHarness(t)
	x.seedBrowseRepo("app", "main")
	session := x.login(x.mintTokenFor(x.admin.ID))

	// The bare repo landing lists the top-level entries at the default branch.
	rec := x.do(http.MethodGet, "/app", nil, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("landing = %d (%s)", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{"hello.go", "sub", "bin.dat"} {
		if !strings.Contains(body, want) {
			t.Fatalf("landing body missing %q:\n%s", want, body)
		}
	}

	// Subdirectory tree.
	rec = x.do(http.MethodGet, "/app/tree/main/sub", nil, session)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "note.txt") {
		t.Fatalf("sub tree = %d (%s)", rec.Code, rec.Body)
	}

	// Text blob renders its content (highlighting splits tokens into spans, so
	// assert on a single token that survives).
	rec = x.do(http.MethodGet, "/app/blob/main/hello.go", nil, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("blob = %d (%s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "package") {
		t.Fatalf("blob body missing content:\n%s", rec.Body)
	}

	// Binary blob shows the download affordance, not garbage.
	rec = x.do(http.MethodGet, "/app/blob/main/bin.dat", nil, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("binary blob = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Binary file") {
		t.Fatalf("binary blob missing download affordance:\n%s", rec.Body)
	}
}

func TestBrowseRawDownload(t *testing.T) {
	x := newHarness(t)
	x.seedBrowseRepo("app", "main")
	session := x.login(x.mintTokenFor(x.admin.ID))

	rec := x.do(http.MethodGet, "/app/raw/main/sub/note.txt", nil, session)
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
	x.seedBrowseRepo("app", "main")
	session := x.login(x.mintTokenFor(x.admin.ID))

	rec := x.do(http.MethodGet, "/app/tree/main", nil, session)
	body := rec.Body.String()
	// The switcher lists both the branch and the tag from meta.ListRefs.
	if !strings.Contains(body, `value="main"`) || !strings.Contains(body, `value="v1"`) {
		t.Fatalf("ref switcher missing branch/tag:\n%s", body)
	}
}

func TestBrowseCommitsAndCommit(t *testing.T) {
	x := newHarness(t)
	head, _ := x.seedBrowseRepo("app", "main")
	session := x.login(x.mintTokenFor(x.admin.ID))

	rec := x.do(http.MethodGet, "/app/commits/main", nil, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("commits = %d (%s)", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "first") || !strings.Contains(body, "second") {
		t.Fatalf("commits body missing subjects:\n%s", body)
	}

	rec = x.do(http.MethodGet, "/app/commit/"+head, nil, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("commit = %d (%s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), head) {
		t.Fatalf("commit body missing sha:\n%s", rec.Body)
	}
}

func TestBrowseHighlightAndMarkdown(t *testing.T) {
	x := newHarness(t)
	x.seedBrowseRepo("app", "main")
	session := x.login(x.mintTokenFor(x.admin.ID))

	// A .go blob is syntax-highlighted: chroma class spans, not a bare <pre>.
	rec := x.do(http.MethodGet, "/app/blob/main/hello.go", nil, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("go blob = %d (%s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `class="`) || !strings.Contains(rec.Body.String(), "chroma") {
		t.Fatalf("go blob not highlighted:\n%s", rec.Body)
	}

	// A .md blob renders markdown in place of highlighted source.
	rec = x.do(http.MethodGet, "/app/blob/main/README.md", nil, session)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<h1") {
		t.Fatalf("md blob not rendered:\n%s", rec.Body)
	}

	// The tree page renders the directory's README below the listing.
	rec = x.do(http.MethodGet, "/app/tree/main", nil, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("tree = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "markdown-body") || !strings.Contains(rec.Body.String(), "<h1") {
		t.Fatalf("tree missing rendered README:\n%s", rec.Body)
	}
}

func TestBrowseMermaid(t *testing.T) {
	x := newHarness(t)
	x.seedBrowseRepo("app", "main")
	session := x.login(x.mintTokenFor(x.admin.ID))

	const script = "/ui/static/mermaid.min.js"

	// A markdown blob with a mermaid fence renders the diagram container AND pulls
	// in the mermaid script.
	rec := x.do(http.MethodGet, "/app/blob/main/diagram.md", nil, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("diagram blob = %d (%s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `class="mermaid"`) {
		t.Fatalf("mermaid container missing:\n%s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), script) {
		t.Fatalf("mermaid script not included on a page with a diagram:\n%s", rec.Body)
	}

	// A markdown blob without a diagram must NOT pull in the script (conditional).
	rec = x.do(http.MethodGet, "/app/blob/main/README.md", nil, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("readme blob = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), script) {
		t.Fatalf("mermaid script included on a page with no diagram:\n%s", rec.Body)
	}

	// The vendored script is served, as javascript, non-empty.
	rec = x.do(http.MethodGet, script, nil, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("mermaid.min.js = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Fatalf("mermaid.min.js content-type = %q, want javascript", ct)
	}
	if rec.Body.Len() == 0 {
		t.Fatal("mermaid.min.js served empty")
	}
}

func TestBrowseBlobSizeGuard(t *testing.T) {
	x := newHarness(t)
	x.seedBrowseRepo("app", "main")
	session := x.login(x.mintTokenFor(x.admin.ID))

	// A blob over the cap falls back to a plain <pre>, no chroma markup.
	rec := x.do(http.MethodGet, "/app/blob/main/big.go", nil, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("big blob = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `class="chroma"`) {
		t.Fatalf("oversize blob was highlighted; expected plain <pre>")
	}
	if !strings.Contains(rec.Body.String(), "<pre>package main") {
		t.Fatalf("oversize blob not shown as plain pre:\n%s", rec.Body.String()[:200])
	}
}

func TestBrowseNotFoundAndTraversal(t *testing.T) {
	x := newHarness(t)
	x.seedBrowseRepo("app", "main")
	session := x.login(x.mintTokenFor(x.admin.ID))

	cases := []struct {
		name, target string
	}{
		{"unknown repo", "/ghost/tree/main"},
		// No leading prefix names a ref, so greedy resolution finds none → 404.
		{"unknown ref", "/app/tree/nope"},
		{"unknown ref with path", "/app/tree/nope/sub"},
		{"missing blob", "/app/blob/main/nope.txt"},
		{"missing tree path", "/app/tree/main/nope"},
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
