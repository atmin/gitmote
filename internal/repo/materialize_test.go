package repo

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/atmin/gitmote/internal/meta"
	"github.com/atmin/gitmote/internal/store"
)

// git runs a git command hermetically (no user/system config, fixed identity)
// and returns its trimmed combined output, failing the test on a nonzero exit.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return gitStdin(t, dir, nil, args...)
}

func gitStdin(t *testing.T, dir string, stdin []byte, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// openMeta returns a fresh local-only metadata DB.
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

// seedRepo builds a real repo with two commits, loads its objects into the
// store under name's prefix, records the repo + branch ref in meta, and returns
// (branch, headSHA). When repack is set the source is packed first, so the
// store holds a packfile instead of loose objects.
func seedRepo(t *testing.T, m *meta.Metadata, s store.Store, name string, repack bool) (string, string) {
	t.Helper()
	ctx := context.Background()

	src := t.TempDir()
	git(t, src, "init", "-b", "main", ".")
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, src, "add", "README.md")
	git(t, src, "commit", "-m", "first")
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, src, "commit", "-am", "second")
	if repack {
		git(t, src, "repack", "-ad")
	}

	branch := git(t, src, "rev-parse", "--abbrev-ref", "HEAD")
	head := git(t, src, "rev-parse", "HEAD")

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
		key := name + "/objects/" + filepath.ToSlash(rel)
		return s.Put(ctx, key, bytes.NewReader(data))
	})
	if err != nil {
		t.Fatalf("seed objects: %v", err)
	}

	r, err := m.CreateRepo(ctx, name, branch)
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	if err := m.CASRef(ctx, r.ID, "refs/heads/"+branch, meta.ZeroSHA, head); err != nil {
		t.Fatalf("CASRef: %v", err)
	}
	return branch, head
}

func TestMaterialize(t *testing.T) {
	for _, tc := range []struct {
		name   string
		repack bool
	}{
		{"loose", false},
		{"packed", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			m := openMeta(t)
			s := store.NewMem()
			const repoName = "dotfiles"
			branch, head := seedRepo(t, m, s, repoName, tc.repack)

			mz := New(m, s, t.TempDir())
			dir, err := mz.Materialize(ctx, repoName)
			if err != nil {
				t.Fatalf("Materialize: %v", err)
			}

			assertRepo(t, dir, branch, head)

			// Cold rebuild: drop the cache dir and rematerialize — must
			// reproduce an equivalent repo at the same path.
			if err := os.RemoveAll(dir); err != nil {
				t.Fatal(err)
			}
			dir2, err := mz.Materialize(ctx, repoName)
			if err != nil {
				t.Fatalf("Materialize (cold rebuild): %v", err)
			}
			if dir2 != dir {
				t.Errorf("rebuild path = %q, want %q", dir2, dir)
			}
			assertRepo(t, dir2, branch, head)
		})
	}
}

// assertRepo checks the materialized repo the way the transport layer will:
// the ref resolves, the head object is present and typed, HEAD tracks the
// branch, and fsck finds no corruption or missing objects.
func assertRepo(t *testing.T, dir, branch, head string) {
	t.Helper()
	if got := git(t, dir, "rev-parse", branch); got != head {
		t.Errorf("rev-parse %s = %q, want %q", branch, got, head)
	}
	check := gitStdin(t, dir, []byte(head+"\n"), "cat-file", "--batch-check")
	if !strings.HasPrefix(check, head+" commit ") {
		t.Errorf("cat-file --batch-check = %q, want %q commit …", check, head)
	}
	if got := git(t, dir, "symbolic-ref", "HEAD"); got != "refs/heads/"+branch {
		t.Errorf("HEAD = %q, want refs/heads/%s", got, branch)
	}
	git(t, dir, "fsck", "--strict") // fails the test on any corruption/missing object
}

// spyStore wraps a Store and counts Get calls, so a test can assert the
// advertisement path never touches an object.
type spyStore struct {
	store.Store
	gets int
}

func (s *spyStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	s.gets++
	return s.Store.Get(ctx, key)
}

// advertise runs `git upload-pack --advertise-refs` against dir — exactly what
// the info/refs GET serves — and returns its output, failing on a nonzero exit.
func advertise(t *testing.T, dir string) string {
	t.Helper()
	return git(t, "", "upload-pack", "--advertise-refs", dir)
}

func TestMaterializeRefsNoObjects(t *testing.T) {
	ctx := context.Background()
	m := openMeta(t)
	s := &spyStore{Store: store.NewMem()}
	const repoName = "dotfiles"
	branch, head := seedRepo(t, m, s, repoName, false)

	// An annotated tag: meta points the ref at the tag object, which is never
	// hydrated. The advertisement must still list it.
	tag := "refs/tags/v1"
	r, err := m.GetRepo(ctx, repoName)
	if err != nil {
		t.Fatalf("GetRepo: %v", err)
	}
	tagSHA := "1234567890123456789012345678901234567890"
	if err := m.CASRef(ctx, r.ID, tag, meta.ZeroSHA, tagSHA); err != nil {
		t.Fatalf("CASRef tag: %v", err)
	}

	mz := New(m, s, t.TempDir())
	s.gets = 0 // seedRepo did no Gets, but be explicit.
	dir, err := mz.MaterializeRefs(ctx, repoName)
	if err != nil {
		t.Fatalf("MaterializeRefs: %v", err)
	}
	if s.gets != 0 {
		t.Errorf("MaterializeRefs called store.Get %d times, want 0", s.gets)
	}

	// The advertisement lists exactly the meta refs and their SHAs, with HEAD
	// tracking the default branch — all with no objects on disk.
	adv := advertise(t, dir)
	for _, want := range []string{
		head + " refs/heads/" + branch,
		tagSHA + " " + tag,
		"symref=HEAD:refs/heads/" + branch,
	} {
		if !strings.Contains(adv, want) {
			t.Errorf("advertisement missing %q\n%s", want, adv)
		}
	}
	// MVP omits the annotated-tag peel line (the tag object is absent).
	if strings.Contains(adv, tag+"^{}") {
		t.Errorf("advertisement unexpectedly carries a peel line for %s\n%s", tag, adv)
	}
	// No object was ever written to disk.
	if entries, err := os.ReadDir(filepath.Join(dir, "objects", "pack")); err != nil {
		t.Fatalf("ReadDir objects/pack: %v", err)
	} else if len(entries) != 0 {
		t.Errorf("objects/pack has %d entries, want 0 (no hydration)", len(entries))
	}
}

func TestMaterializeUnknownRepo(t *testing.T) {
	mz := New(openMeta(t), store.NewMem(), t.TempDir())
	if _, err := mz.Materialize(context.Background(), "nope"); !errors.Is(err, meta.ErrNotFound) {
		t.Fatalf("Materialize(unknown) = %v, want meta.ErrNotFound", err)
	}
}

// TestRepoDirRejectsUnsafeName guards the cache-root escape defense directly:
// CreateRepo now forbids "/" in a name, so such a name can't reach the
// materializer through meta, but repoDir stays defense-in-depth against any name
// that would resolve outside the cache root.
func TestRepoDirRejectsUnsafeName(t *testing.T) {
	mz := New(openMeta(t), store.NewMem(), t.TempDir())
	for _, name := range []string{"../evil", "/etc/passwd", "a/../../b", ""} {
		if _, err := mz.repoDir(name); err == nil {
			t.Errorf("repoDir(%q) succeeded, want unsafe-name error", name)
		}
	}
}
