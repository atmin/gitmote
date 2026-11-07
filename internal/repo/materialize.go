// Package repo materializes bare git repositories on local disk from the
// durable sources of truth. Refs come from the metadata layer, and objects
// come from the object store. A materialized repo is a disposable cache on
// ephemeral disk (docs/architecture/storage.md). It is never authoritative:
// deleting the cache directory and rebuilding must give an equivalent repo.
package repo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/atmin/gitmote/internal/meta"
	"github.com/atmin/gitmote/internal/store"
)

// Materializer builds and refreshes cached bare repos under a root directory.
type Materializer struct {
	meta  *meta.Metadata
	store store.Store
	root  string
}

// New returns a Materializer that caches repos under root.
func New(m *meta.Metadata, s store.Store, root string) *Materializer {
	return &Materializer{meta: m, store: s, root: root}
}

// Root is the cache directory under which repos materialize. A repo named
// "app" lands at Root()/app. It is the GIT_PROJECT_ROOT of git-http-backend.
func (mz *Materializer) Root() string { return mz.root }

// Materialize ensures that a valid bare repo for name exists on disk under the
// cache root, and returns its path. It does these steps:
//   - It creates the repo on a cache miss (rebuild on miss).
//   - It hydrates the object closure from the store.
//   - It writes refs from the metadata layer.
//
// So a warm repo is refreshed to the current refs, and a cold repo is built
// from scratch. It returns meta.ErrNotFound if the repo is unknown.
//
// The hydration policy is full hydration (see docs/architecture/storage.md).
// It pulls every object under the store prefix of the repo. This is the path
// for the data POSTs (upload-pack or receive-pack) that transfer history.
// MaterializeRefs serves the info/refs advertisement with no objects.
//
// The repo of gitmote itself is tiny. So full hydration on the data path is
// safe and simple. Bounded closures per operation are not implemented.
func (mz *Materializer) Materialize(ctx context.Context, name string) (string, error) {
	r, err := mz.meta.GetRepo(ctx, name)
	if err != nil {
		return "", err
	}

	dir, err := mz.ensureRepo(ctx, name)
	if err != nil {
		return "", err
	}

	if err := mz.hydrateObjects(ctx, name, dir); err != nil {
		return "", err
	}
	if err := mz.writeRefs(ctx, dir, r); err != nil {
		return "", err
	}
	return dir, nil
}

// MaterializeRefs ensures that a bare repo for name exists, and makes its ref
// advertisement current. It points HEAD at the default branch. It returns
// meta.ErrNotFound if the repo is unknown.
//
// It does not hydrate a single object. It writes packed-refs directly from the
// metadata layer. It does not use update-ref, which requires each target
// object to be present.
//
// This serves the info/refs GET, which lists refs but transfers no history. It
// is a case of bounded hydration.
//
// It omits the ^{} peel lines of annotated tags. Git still advertises the tag
// ref and only drops the peel hint. That loses a negotiation optimization,
// but it is not a correctness issue. Like Materialize, it mirrors the metadata
// refs exactly (see mirrorRefs). So no loose ref left on disk can override
// them.
func (mz *Materializer) MaterializeRefs(ctx context.Context, name string) (string, error) {
	r, err := mz.meta.GetRepo(ctx, name)
	if err != nil {
		return "", err
	}

	dir, err := mz.ensureRepo(ctx, name)
	if err != nil {
		return "", err
	}

	refs, err := mz.meta.ListRefs(ctx, r.ID)
	if err != nil {
		return "", err
	}
	if err := mirrorRefs(dir, refs); err != nil {
		return "", err
	}
	return dir, setHEAD(ctx, dir, r.DefaultBranch)
}

// ensureRepo maps name to its cache directory and guarantees that a bare repo
// exists there. It creates one on a cache miss (rebuild on miss). It touches no
// refs or objects.
func (mz *Materializer) ensureRepo(ctx context.Context, name string) (string, error) {
	dir, err := mz.repoDir(name)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return "", err
		}
		if err := runGit(ctx, "", "init", "--bare", dir); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	return dir, nil
}

// repoDir maps a repo name to its cache path. It rejects a name that would
// escape the cache root: an absolute path, "..", or any name that
// filepath.Clean rewrites.
func (mz *Materializer) repoDir(name string) (string, error) {
	if name == "" || filepath.IsAbs(name) ||
		filepath.ToSlash(filepath.Clean(name)) != name ||
		strings.HasPrefix(name, "../") {
		return "", fmt.Errorf("repo: unsafe repo name %q", name)
	}
	return filepath.Join(mz.root, filepath.FromSlash(name)), nil
}

// hydrateConcurrency bounds the parallel object copies during hydration. Each
// missing object is one S3 GET. This is the cost of a cold start or a first
// request. Concurrent fetches turn N serial round trips into
// N/hydrateConcurrency. The value is modest, to bound the open sockets and
// file descriptors for a large repo.
const hydrateConcurrency = 16

// hydrateObjects copies every object under the store prefix of the repo onto
// disk. It mirrors the {repo}/objects/… layout of the store into the objects/
// directory of the bare repo. Objects are immutable and content-addressed. So
// an object already on disk is left untouched, and the copies are independent.
// Up to hydrateConcurrency copies run in parallel. A copy error cancels the
// group and aborts the rest.
func (mz *Materializer) hydrateObjects(ctx context.Context, name, dir string) error {
	prefix := name + "/objects/"
	keys, err := mz.store.List(ctx, prefix)
	if err != nil {
		return err
	}
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(hydrateConcurrency)
	for _, key := range keys {
		rel := strings.TrimPrefix(key, name+"/") // for example "objects/ab/cdef…"
		dst := filepath.Join(dir, filepath.FromSlash(rel))
		if _, err := os.Stat(dst); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		g.Go(func() error { return mz.copyObject(ctx, key, dst) })
	}
	return g.Wait()
}

// copyObject streams one store object to dst. It writes to a temp file first,
// and then renames it into place. So a reader never observes a partial object.
func (mz *Materializer) copyObject(ctx context.Context, key, dst string) error {
	rc, err := mz.store.Get(ctx, key)
	if err != nil {
		return err
	}
	defer rc.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".hydrate-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := io.Copy(tmp, rc); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// writeRefs makes the on-disk refs mirror the metadata layer (see mirrorRefs).
// It points HEAD at the default branch of the repo. Hydration runs first, so
// the object behind every ref is on disk.
func (mz *Materializer) writeRefs(ctx context.Context, dir string, r *meta.Repo) error {
	refs, err := mz.meta.ListRefs(ctx, r.ID)
	if err != nil {
		return err
	}
	if err := mirrorRefs(dir, refs); err != nil {
		return err
	}
	return setHEAD(ctx, dir, r.DefaultBranch)
}

// mirrorRefs makes the on-disk refs equal the refs of the metadata layer. It
// writes packed-refs from refs, and then removes every loose ref.
//
// A loose ref overrides packed-refs. Without the removal, a ref deleted in the
// metadata layer would stay on disk. After a lease handoff, the pushes of the
// other instance can make a cache stale. That cache would then advertise a ref
// that has moved or does not exist.
//
// packed-refs is written first. So a concurrent reader sees the old loose
// value or the new packed value, never neither.
func mirrorRefs(dir string, refs []meta.Ref) error {
	if err := writePackedRefs(dir, refs); err != nil {
		return err
	}
	return removeLooseRefs(dir)
}

// removeLooseRefs deletes every loose ref file under dir/refs. It keeps the
// directories and any *.lock file, which belongs to a git process at work. It
// ignores an entry that vanishes during the walk.
func removeLooseRefs(dir string) error {
	return filepath.WalkDir(filepath.Join(dir, "refs"), func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() || strings.HasSuffix(d.Name(), ".lock") {
			return nil
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	})
}

// writePackedRefs overwrites the packed-refs file of the repo from refs. The
// write is atomic: a temp file and a rename. So a concurrent reader never sees
// a torn file.
//
// Each line is "<sha> <refname>". It writes no ^{} peel lines and no traits
// header. So git peels from the objects when they are present. When they are
// absent, git omits the peel hint.
//
// The advertisement without objects needs exactly this behavior. Git
// advertises refs whose target objects are absent, and the advertisement
// without objects depends on that.
func writePackedRefs(dir string, refs []meta.Ref) error {
	var b strings.Builder
	for _, ref := range refs {
		b.WriteString(ref.SHA)
		b.WriteByte(' ')
		b.WriteString(ref.Name)
		b.WriteByte('\n')
	}
	return writeFileAtomic(filepath.Join(dir, "packed-refs"), []byte(b.String()))
}

// setHEAD points the HEAD of the repo at the default branch. HEAD is derived
// from default_branch, and is not stored as a ref (storage.md). symbolic-ref
// accepts a branch that has no commits yet.
func setHEAD(ctx context.Context, dir, defaultBranch string) error {
	return runGit(ctx, dir, "symbolic-ref", "HEAD", path.Join("refs/heads", defaultBranch))
}

// writeFileAtomic writes data to path through a temp file in the same
// directory, followed by a rename. So a reader observes either the old file or
// the complete new file, never a partial write.
func writeFileAtomic(dst string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".packed-refs-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// runGit runs a git subcommand, using dir as the working directory when set.
func runGit(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
