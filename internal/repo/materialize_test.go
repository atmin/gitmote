package repo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/atmin/gitmote/internal/meta"
	"github.com/atmin/gitmote/internal/store"
)

// git runs a git command hermetically, with no user or system config and a
// fixed identity. It returns the trimmed combined output. It fails the test on
// a nonzero exit.
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

// seedRepo builds a real repo with two commits. It loads the objects into the
// store under the prefix of name. It records the repo and the branch ref in
// meta, and returns (branch, headSHA). When repack is set, the source is
// packed first. The store then holds a packfile instead of loose objects.
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

			// A cold rebuild: drop the cache directory and materialize again.
			// It must reproduce an equivalent repo at the same path.
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

// assertRepo checks the materialized repo the way the transport layer does:
//   - The ref resolves.
//   - The head object is present and typed.
//   - HEAD tracks the branch.
//   - fsck finds no corruption or missing objects.
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
	git(t, dir, "fsck", "--strict") // fails the test on any corruption or missing object
}

// spyStore wraps a Store and counts Get calls. A test can then assert that the
// advertisement path never touches an object.
type spyStore struct {
	store.Store
	gets int
}

func (s *spyStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	s.gets++
	return s.Store.Get(ctx, key)
}

// advertise runs `git upload-pack --advertise-refs` against dir. That is
// exactly what the info/refs GET serves. It returns the output, and fails on a
// nonzero exit.
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

	// An annotated tag. Meta points the ref at the tag object, which is never
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

	// The advertisement lists exactly the meta refs and their SHAs. HEAD tracks
	// the default branch. There are no objects on disk.
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
	// The peel line of the annotated tag is omitted, because the tag object is
	// absent.
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

// TestMaterializeMirrorsMetaRefs covers a cache that another instance made
// stale. After a lease handoff, the other instance moved one branch and
// deleted another. This cache still holds the loose refs that its own
// receive-pack wrote. The advertisement and the full materialize must both
// show exactly the metadata refs.
func TestMaterializeMirrorsMetaRefs(t *testing.T) {
	for _, tc := range []struct {
		name        string
		materialize func(*Materializer, context.Context, string) (string, error)
	}{
		{"MaterializeRefs", (*Materializer).MaterializeRefs},
		{"Materialize", (*Materializer).Materialize},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			m := openMeta(t)
			s := store.NewMem()
			const repoName = "dotfiles"
			branch, head := seedRepo(t, m, s, repoName, false)
			r, err := m.GetRepo(ctx, repoName)
			if err != nil {
				t.Fatalf("GetRepo: %v", err)
			}
			const doomed = "refs/heads/doomed"
			if err := m.CASRef(ctx, r.ID, doomed, meta.ZeroSHA, head); err != nil {
				t.Fatalf("CASRef doomed: %v", err)
			}

			// This instance materializes, and its receive-pack leaves loose
			// refs.
			mz := New(m, s, t.TempDir())
			dir, err := mz.Materialize(ctx, repoName)
			if err != nil {
				t.Fatalf("Materialize: %v", err)
			}
			git(t, dir, "update-ref", "refs/heads/"+branch, head)
			git(t, dir, "update-ref", doomed, head)

			// The other instance moves the branch back one commit and deletes doomed.
			parent := git(t, dir, "rev-parse", head+"~1")
			if err := m.CASRef(ctx, r.ID, "refs/heads/"+branch, head, parent); err != nil {
				t.Fatalf("CASRef move: %v", err)
			}
			if err := m.CASRef(ctx, r.ID, doomed, head, meta.ZeroSHA); err != nil {
				t.Fatalf("CASRef delete: %v", err)
			}

			if _, err := tc.materialize(mz, ctx, repoName); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if got, want := git(t, dir, "for-each-ref", "--format=%(objectname) %(refname)"), parent+" refs/heads/"+branch; got != want {
				t.Errorf("refs on disk = %q, want exactly %q", got, want)
			}
			adv := advertise(t, dir)
			if !strings.Contains(adv, parent+" refs/heads/"+branch) {
				t.Errorf("advertisement does not carry the moved branch at %s\n%s", parent, adv)
			}
			if strings.Contains(adv, doomed) {
				t.Errorf("advertisement still carries the deleted %s\n%s", doomed, adv)
			}
		})
	}
}

// afterListStore runs hook once, after the first List. It stands in for a
// concurrent request whose refresh lands a push between hydration and the ref
// read.
type afterListStore struct {
	store.Store
	hook func()
}

func (s *afterListStore) List(ctx context.Context, prefix string) ([]string, error) {
	keys, err := s.Store.List(ctx, prefix)
	if s.hook != nil {
		hook := s.hook
		s.hook = nil
		hook()
	}
	return keys, err
}

// TestMaterializeRefsHaveObjects covers a follower whose metadata advances
// during a materialize. A push lands after the hydration lists the store. The
// cache must not write a ref whose objects it never hydrated.
func TestMaterializeRefsHaveObjects(t *testing.T) {
	ctx := context.Background()
	m := openMeta(t)
	s := &afterListStore{Store: store.NewMem()}
	const repoName = "dotfiles"
	seedRepo(t, m, s, repoName, false)
	r, err := m.GetRepo(ctx, repoName)
	if err != nil {
		t.Fatalf("GetRepo: %v", err)
	}

	// The late push is a commit with its own content, packed into one pack.
	src := t.TempDir()
	git(t, src, "init", "-b", "late", ".")
	if err := os.WriteFile(filepath.Join(src, "late.txt"), []byte("late\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, src, "add", "late.txt")
	git(t, src, "commit", "-m", "late")
	git(t, src, "repack", "-ad")
	late := git(t, src, "rev-parse", "HEAD")
	packs, err := filepath.Glob(filepath.Join(src, ".git", "objects", "pack", "*"))
	if err != nil || len(packs) == 0 {
		t.Fatalf("late pack files: %v %v", packs, err)
	}
	s.hook = func() {
		// Content first, then the pointer.
		for _, p := range packs {
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatalf("read %s: %v", p, err)
			}
			if err := s.Put(ctx, repoName+"/objects/pack/"+filepath.Base(p), bytes.NewReader(data)); err != nil {
				t.Fatalf("Put %s: %v", p, err)
			}
		}
		if err := m.CASRef(ctx, r.ID, "refs/heads/late", meta.ZeroSHA, late); err != nil {
			t.Fatalf("CASRef late: %v", err)
		}
	}

	dir, err := New(m, s, t.TempDir()).Materialize(ctx, repoName)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	for _, line := range strings.Split(git(t, dir, "for-each-ref", "--format=%(objectname) %(refname)"), "\n") {
		sha, ref, _ := strings.Cut(line, " ")
		cmd := exec.Command("git", "cat-file", "-e", sha)
		cmd.Dir = dir
		if err := cmd.Run(); err != nil {
			t.Errorf("%s points at %s, which is not on disk", ref, sha)
		}
	}
}

func TestMaterializeUnknownRepo(t *testing.T) {
	mz := New(openMeta(t), store.NewMem(), t.TempDir())
	if _, err := mz.Materialize(context.Background(), "nope"); !errors.Is(err, meta.ErrNotFound) {
		t.Fatalf("Materialize(unknown) = %v, want meta.ErrNotFound", err)
	}
}

// TestRepoDirRejectsUnsafeName tests directly the defense against an escape
// from the cache root. CreateRepo forbids "/" in a name. So such a name can't
// reach the materializer through meta. But repoDir stays as defense in depth
// against any name that would resolve outside the cache root.
func TestRepoDirRejectsUnsafeName(t *testing.T) {
	mz := New(openMeta(t), store.NewMem(), t.TempDir())
	for _, name := range []string{"../evil", "/etc/passwd", "a/../../b", ""} {
		if _, err := mz.repoDir(name); err == nil {
			t.Errorf("repoDir(%q) succeeded, want unsafe-name error", name)
		}
	}
}

// TestMaterializeManyObjectsParallel drives hydrateObjects with far more loose
// objects than hydrateConcurrency, so the bounded pool queues. It asserts that
// the concurrent copies still reproduce a correct repo at the right head:
//   - The repo is byte-identical.
//   - fsck finds it clean.
//   - It has no torn objects and no lost keys.
func TestMaterializeManyObjectsParallel(t *testing.T) {
	ctx := context.Background()
	m := openMeta(t)
	s := store.NewMem()
	const repoName = "many"

	// One commit that touches 60 distinct files gives 60 blobs, the trees, and a
	// commit. That is well past hydrateConcurrency (16). It forces the pool to
	// block and drain.
	src := t.TempDir()
	git(t, src, "init", "-b", "main", ".")
	for i := 0; i < 60; i++ {
		name := fmt.Sprintf("f%02d.txt", i)
		if err := os.WriteFile(filepath.Join(src, name), []byte(fmt.Sprintf("content %d\n", i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, src, "add", "-A")
	git(t, src, "commit", "-m", "many files") // no repack, so objects stay loose
	branch := git(t, src, "rev-parse", "--abbrev-ref", "HEAD")
	head := git(t, src, "rev-parse", "HEAD")

	objRoot := filepath.Join(src, ".git", "objects")
	loose := 0
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
		loose++
		return s.Put(ctx, repoName+"/objects/"+filepath.ToSlash(rel), bytes.NewReader(data))
	})
	if err != nil {
		t.Fatalf("seed objects: %v", err)
	}
	if loose <= hydrateConcurrency {
		t.Fatalf("seeded %d objects, need > hydrateConcurrency (%d) to exercise queueing", loose, hydrateConcurrency)
	}

	r, err := m.CreateRepo(ctx, repoName, branch)
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	if err := m.CASRef(ctx, r.ID, "refs/heads/"+branch, meta.ZeroSHA, head); err != nil {
		t.Fatalf("CASRef: %v", err)
	}

	dir, err := New(m, s, t.TempDir()).Materialize(ctx, repoName)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	assertRepo(t, dir, branch, head)
}
