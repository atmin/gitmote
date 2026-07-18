package repo

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// browseFixture builds a real repo with two commits, a subdirectory, a binary
// blob, and a tag, and returns its dir plus the two commit SHAs (first, head).
func browseFixture(t *testing.T) (dir, first, head string) {
	t.Helper()
	dir = t.TempDir()
	git(t, dir, "init", "-b", "main", ".")

	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "file.txt"), []byte("nested\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin.dat"), []byte{0x00, 0x01, 0x02, 0x00}, 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-m", "first")
	first = git(t, dir, "rev-parse", "HEAD")

	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "commit", "-am", "second")
	head = git(t, dir, "rev-parse", "HEAD")

	git(t, dir, "tag", "v1")
	return dir, first, head
}

func TestResolveRef(t *testing.T) {
	ctx := context.Background()
	dir, _, head := browseFixture(t)

	for _, ref := range []string{"main", "v1", head} {
		got, err := ResolveRef(ctx, dir, ref)
		if err != nil {
			t.Fatalf("ResolveRef(%q): %v", ref, err)
		}
		if got != head {
			t.Fatalf("ResolveRef(%q) = %q, want %q", ref, got, head)
		}
	}

	if _, err := ResolveRef(ctx, dir, "--all"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ResolveRef(flag) err = %v, want ErrNotFound", err)
	}
	if _, err := ResolveRef(ctx, dir, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ResolveRef(unknown) err = %v, want ErrNotFound", err)
	}
}

func TestTree(t *testing.T) {
	ctx := context.Background()
	dir, _, head := browseFixture(t)

	root, err := Tree(ctx, dir, head, "")
	if err != nil {
		t.Fatalf("Tree(root): %v", err)
	}
	types := map[string]string{}
	for _, e := range root {
		types[e.Name] = e.Type
	}
	if types["README.md"] != "blob" || types["sub"] != "tree" || types["bin.dat"] != "blob" {
		t.Fatalf("root entries = %+v", root)
	}

	sub, err := Tree(ctx, dir, head, "sub")
	if err != nil {
		t.Fatalf("Tree(sub): %v", err)
	}
	if len(sub) != 1 || sub[0].Path != "sub/file.txt" || sub[0].Name != "file.txt" {
		t.Fatalf("sub entries = %+v", sub)
	}

	if _, err := Tree(ctx, dir, head, "../etc"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Tree(traversal) err = %v, want ErrNotFound", err)
	}
}

func TestBlob(t *testing.T) {
	ctx := context.Background()
	dir, _, head := browseFixture(t)

	content, size, binary, err := Blob(ctx, dir, head, "README.md")
	if err != nil {
		t.Fatalf("Blob(README): %v", err)
	}
	if binary || size != int64(len(content)) || string(content) != "hello\nworld\n" {
		t.Fatalf("README blob: content=%q size=%d binary=%v", content, size, binary)
	}

	_, _, binary, err = Blob(ctx, dir, head, "bin.dat")
	if err != nil {
		t.Fatalf("Blob(bin): %v", err)
	}
	if !binary {
		t.Fatal("bin.dat not flagged binary")
	}

	if _, _, _, err := Blob(ctx, dir, head, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Blob(missing) err = %v, want ErrNotFound", err)
	}
}

func TestBlobReaderAndSize(t *testing.T) {
	ctx := context.Background()
	dir, _, head := browseFixture(t)

	size, err := BlobSize(ctx, dir, head, "README.md")
	if err != nil {
		t.Fatalf("BlobSize: %v", err)
	}
	if size != int64(len("hello\nworld\n")) {
		t.Fatalf("BlobSize = %d", size)
	}

	rc, err := BlobReader(ctx, dir, head, "README.md")
	if err != nil {
		t.Fatalf("BlobReader: %v", err)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("close blob: %v", err)
	}
	if string(got) != "hello\nworld\n" {
		t.Fatalf("streamed blob = %q", got)
	}

	if _, err := BlobSize(ctx, dir, head, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("BlobSize(missing) err = %v, want ErrNotFound", err)
	}
}

func TestLogOverflowMarker(t *testing.T) {
	ctx := context.Background()
	dir, _, head := browseFixture(t)

	commits, more, err := Log(ctx, dir, head, "", 1)
	if err != nil {
		t.Fatalf("Log(limit 1): %v", err)
	}
	if len(commits) != 1 || !more {
		t.Fatalf("Log(limit 1) = %d commits, more=%v; want 1, true", len(commits), more)
	}
	if commits[0].Subject != "second" || commits[0].Author == "" || commits[0].When.IsZero() {
		t.Fatalf("head commit = %+v", commits[0])
	}

	all, more, err := Log(ctx, dir, head, "", 10)
	if err != nil {
		t.Fatalf("Log(limit 10): %v", err)
	}
	if len(all) != 2 || more {
		t.Fatalf("Log(limit 10) = %d commits, more=%v; want 2, false", len(all), more)
	}

	scoped, _, err := Log(ctx, dir, head, "sub/file.txt", 10)
	if err != nil {
		t.Fatalf("Log(path): %v", err)
	}
	if len(scoped) != 1 || scoped[0].Subject != "first" {
		t.Fatalf("Log(path) = %+v", scoped)
	}
}

func TestShow(t *testing.T) {
	ctx := context.Background()
	dir, first, head := browseFixture(t)

	c, diff, err := Show(ctx, dir, head)
	if err != nil {
		t.Fatalf("Show(head): %v", err)
	}
	if c.SHA != head || c.Subject != "second" {
		t.Fatalf("Show(head) commit = %+v", c)
	}
	if !strings.Contains(diff, "+world") {
		t.Fatalf("Show(head) diff missing added line:\n%s", diff)
	}

	// --root makes the initial commit diff against the empty tree.
	_, diff, err = Show(ctx, dir, first)
	if err != nil {
		t.Fatalf("Show(first): %v", err)
	}
	if !strings.Contains(diff, "+hello") {
		t.Fatalf("Show(first) diff missing initial content:\n%s", diff)
	}
}

func TestCompare(t *testing.T) {
	ctx := context.Background()
	dir, first, head := browseFixture(t)

	// first..head: the second commit is the one head adds, and its diff adds "world".
	commits, diff, err := Compare(ctx, dir, first, head)
	if err != nil {
		t.Fatalf("Compare(first, head): %v", err)
	}
	if len(commits) != 1 || commits[0].SHA != head || commits[0].Subject != "second" {
		t.Fatalf("Compare(first, head) commits = %+v, want just the second commit", commits)
	}
	if !strings.Contains(diff, "+world") {
		t.Fatalf("Compare(first, head) diff missing added line:\n%s", diff)
	}

	// Identical revs: no commits, empty diff (not an error).
	commits, diff, err = Compare(ctx, dir, head, head)
	if err != nil {
		t.Fatalf("Compare(head, head): %v", err)
	}
	if len(commits) != 0 || diff != "" {
		t.Fatalf("Compare(head, head) = %d commits, diff %q; want empty", len(commits), diff)
	}

	// An unknown rev is ErrNotFound, not a launch error.
	if _, _, err := Compare(ctx, dir, first, "deadbeefdeadbeef"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Compare(unknown head) err = %v, want ErrNotFound", err)
	}
}
