// Package repo materializes on-disk bare git repositories from the durable
// sources of truth: refs from the metadata layer, objects from the object
// store. A materialized repo is a disposable cache (docs/architecture/storage.md
// → ephemeral disk), never authoritative — deleting the cache dir and
// rebuilding must reproduce an equivalent repo.
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

// Root is the cache directory under which repos materialize; a repo named
// "a/b" lands at Root()/a/b. It is the git-http-backend GIT_PROJECT_ROOT.
func (mz *Materializer) Root() string { return mz.root }

// Materialize ensures a valid on-disk bare repo for name exists under the cache
// root and returns its path. It creates the repo on a cache miss (rebuild-on-
// miss), hydrates the object closure from the store, and writes refs from the
// metadata layer — so a warm repo is refreshed to the current refs and a cold
// one is built from scratch. Returns meta.ErrNotFound if the repo is unknown.
//
// Hydration policy is full-hydrate for the MVP (see task 04 /
// docs/notes/object-hydration.md): every object under the repo's store prefix
// is pulled. This is the path for the data POSTs (upload-pack / receive-pack)
// that actually transfer history; the info/refs advertisement is served
// objectless via MaterializeRefs. gitmote's own repo is tiny, so full-hydrate
// on the data path is the safe, simple first cut; bounded per-operation
// closures are a later optimization.
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

// MaterializeRefs ensures a bare repo for name exists and makes its ref
// advertisement current — without hydrating a single object. It writes
// packed-refs directly from the metadata layer (not update-ref, which requires
// each target object present) and points HEAD at the default branch. This
// serves the info/refs GET, which lists refs but transfers no history — the
// first slice of bounded hydration (docs/notes/object-hydration.md). Returns
// meta.ErrNotFound if the repo is unknown.
//
// MVP omits annotated-tag ^{} peel lines: git still advertises the tag ref and
// merely drops the peel hint (a lost negotiation optimization, not a
// correctness issue). A refs-only packed-refs and a later full Materialize's
// loose refs coexist on disk — loose refs override, which is correct.
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
	if err := writePackedRefs(dir, refs); err != nil {
		return "", err
	}
	return dir, setHEAD(ctx, dir, r.DefaultBranch)
}

// ensureRepo maps name to its cache dir and guarantees a bare repo exists there,
// creating one on a cache miss (rebuild-on-miss). It touches no refs or objects.
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

// repoDir maps a repo name to its cache path, rejecting names that would escape
// the cache root (absolute paths, "..", or anything filepath.Clean rewrites).
func (mz *Materializer) repoDir(name string) (string, error) {
	if name == "" || filepath.IsAbs(name) ||
		filepath.ToSlash(filepath.Clean(name)) != name ||
		strings.HasPrefix(name, "../") {
		return "", fmt.Errorf("repo: unsafe repo name %q", name)
	}
	return filepath.Join(mz.root, filepath.FromSlash(name)), nil
}

// hydrateObjects copies every object under the repo's store prefix onto disk,
// mirroring the store's {repo}/objects/… layout into the bare repo's objects/
// dir. Objects are immutable and content-addressed, so an object already on
// disk is left untouched.
func (mz *Materializer) hydrateObjects(ctx context.Context, name, dir string) error {
	prefix := name + "/objects/"
	keys, err := mz.store.List(ctx, prefix)
	if err != nil {
		return err
	}
	for _, key := range keys {
		rel := strings.TrimPrefix(key, name+"/") // e.g. "objects/ab/cdef…"
		dst := filepath.Join(dir, filepath.FromSlash(rel))
		if _, err := os.Stat(dst); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := mz.copyObject(ctx, key, dst); err != nil {
			return err
		}
	}
	return nil
}

// copyObject streams one store object to dst, writing to a temp file first and
// renaming into place so a reader never observes a partial object.
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

// writeRefs makes the on-disk refs match the metadata layer and points HEAD at
// the repo's default branch. update-ref validates that each target object is
// present, so hydration must run first.
func (mz *Materializer) writeRefs(ctx context.Context, dir string, r *meta.Repo) error {
	refs, err := mz.meta.ListRefs(ctx, r.ID)
	if err != nil {
		return err
	}
	for _, ref := range refs {
		if err := runGit(ctx, dir, "update-ref", ref.Name, ref.SHA); err != nil {
			return err
		}
	}
	return setHEAD(ctx, dir, r.DefaultBranch)
}

// writePackedRefs overwrites the repo's packed-refs file from refs, atomically
// (temp file + rename, so a concurrent reader never sees a torn file). Each line
// is "<sha> <refname>"; the MVP writes no ^{} peel lines and no traits header,
// so git peels from the objects when present and simply omits the peel hint when
// they are absent — exactly the objectless-advertisement behavior we want. git
// advertises refs whose target objects are absent, which is the whole point.
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

// setHEAD points the repo's HEAD at the default branch. HEAD is derived from
// default_branch, not stored as a ref (storage.md); symbolic-ref is happy to
// point at a branch that has no commits yet.
func setHEAD(ctx context.Context, dir, defaultBranch string) error {
	return runGit(ctx, dir, "symbolic-ref", "HEAD", path.Join("refs/heads", defaultBranch))
}

// writeFileAtomic writes data to path via a temp file in the same directory
// followed by a rename, so a reader observes either the old file or the complete
// new one, never a partial write.
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
