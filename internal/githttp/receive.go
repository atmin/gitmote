package githttp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/atmin/gitmote/internal/hookrpc"
	"github.com/atmin/gitmote/internal/meta"
	"github.com/atmin/gitmote/internal/store"
)

// casTimeout bounds the object PUT and the ref CAS that the parent runs for one
// push.
const casTimeout = 2 * time.Minute

// Writer is the coordinator of the write path, shared by all repos. It owns
// these:
//   - the unix socket that the pre-receive hook calls back on
//   - the per-repo write locks that serialize pushes
//   - the single-use nonces that bind each hook call to its in-flight push
//
// On a hook call, it enforces the safety-critical order (safety.md §3). First,
// the objects become durable in S3. Second, the ref CAS runs in s3lite.
type Writer struct {
	meta   *meta.Metadata
	store  store.Store
	logger *slog.Logger

	hookDir  string // core.hooksPath: holds the pre-receive executable
	sockPath string
	listener net.Listener

	locksMu sync.Mutex
	locks   map[string]*sync.Mutex

	pushMu sync.Mutex
	pushes map[string]pushOp // maps a nonce to its in-flight push

	// beforeCAS, when set, runs after the object PUT and before the ref CAS. It
	// is a test seam for the ordering invariant. A non-nil error rejects the
	// push after the objects are already durable.
	beforeCAS func() error

	// AfterCommit, when set, runs once for each push that advanced refs. The
	// HTTP handler fires it after receive-pack has fully finished (see
	// FireAfterCommit). So the on-disk repo already reflects the new refs. It
	// is fire-and-forget. The push has already committed, so a panic or a
	// delay must not fail the push. This is content-before-pointer applied to
	// CI (safety.md §3). It receives these:
	//   - the user id of the authenticated pusher. The CI clone token is
	//     minted under it, because the pusher necessarily holds repo read.
	//   - one CommitInfo for each updated ref
	AfterCommit func(ctx context.Context, pusherID int64, commits []CommitInfo)
}

// CommitInfo describes one ref that an accepted push advanced. It carries the
// repo identity, so the after-commit hook is self-contained. A push targets
// one repo. So RepoID and RepoName repeat across the entries of a multi-ref
// push.
type CommitInfo struct {
	RepoID   int64
	RepoName string
	Ref      string
	Old      string
	New      string
}

// afterCommitTimeout bounds the fire-and-forget after-commit hook, so that a
// slow dispatch can't hold the push handler open indefinitely.
const afterCommitTimeout = 30 * time.Second

// pushOp is the context in the parent that a nonce resolves to. defaultBranch
// and pusherIsAdmin are the inputs to the force-push guard of the default
// branch (docs/architecture/urls.md). Only an admin may force-push or delete
// the default branch. So the guard needs the branch to protect and the level
// of the pusher.
type pushOp struct {
	repoID        int64
	repoName      string
	defaultBranch string
	pusherIsAdmin bool
	outcome       *pushOutcome
}

// pushOutcome collects the refs that a push committed. The hook callback
// (Writer.handle) fills it after a successful CAS. The HTTP handler reads it
// after receive-pack finishes, to fire the after-commit hook. Its mutex guards
// it, so the write in the socket goroutine is visible to the read in the
// request goroutine.
type pushOutcome struct {
	mu      sync.Mutex
	commits []CommitInfo
}

func (o *pushOutcome) record(commits []CommitInfo) {
	o.mu.Lock()
	o.commits = commits
	o.mu.Unlock()
}

func (o *pushOutcome) committed() []CommitInfo {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.commits
}

// NewWriter starts the write coordinator. It installs the pre-receive hook as
// a symlink to hookBinary under a private hooks directory. It then begins to
// listen on sockPath. Call Close to stop the listener and remove the socket.
func NewWriter(md *meta.Metadata, s store.Store, hookBinary, sockPath string, logger *slog.Logger) (*Writer, error) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	hookDir, err := installHook(hookBinary)
	if err != nil {
		return nil, err
	}

	// A stale socket from a crashed predecessor would block Listen.
	_ = os.Remove(sockPath)
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o755); err != nil {
		return nil, err
	}
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("githttp: listen %s: %w", sockPath, err)
	}

	w := &Writer{
		meta:     md,
		store:    s,
		logger:   logger,
		hookDir:  hookDir,
		sockPath: sockPath,
		listener: l,
		locks:    make(map[string]*sync.Mutex),
		pushes:   make(map[string]pushOp),
	}
	go hookrpc.Serve(l, w.handle)
	return w, nil
}

// Close stops the socket listener and removes the socket file.
func (w *Writer) Close() error {
	err := w.listener.Close()
	_ = os.Remove(w.sockPath)
	return err
}

// HooksPath is the core.hooksPath value for spawned receive-pack processes.
func (w *Writer) HooksPath() string { return w.hookDir }

// SockPath is the unix socket that the hook dials.
func (w *Writer) SockPath() string { return w.sockPath }

// Push is an in-flight push. Its single-use Nonce goes to the hook of
// receive-pack. Release drops the per-repo lock and burns the nonce.
type Push struct {
	Nonce   string
	outcome *pushOutcome
	release func()
}

// Release ends the push, unlocking the repo and discarding the nonce.
func (p *Push) Release() { p.release() }

// Committed returns the refs that this push advanced. They are available once
// receive-pack has run. It is empty for a rejected or no-op push.
func (p *Push) Committed() []CommitInfo { return p.outcome.committed() }

// Begin starts a push. It does these steps:
//   - It resolves the repo.
//   - It takes the per-repo write lock, so pushes to one repo serialize.
//   - It mints a nonce bound to this operation.
//
// It also resolves the ACL level of the pusher. The hook callback then can
// apply the force-push guard of the default branch, which is admin-only. The
// caller must Release the returned Push. Begin returns meta.ErrNotFound for an
// unknown repo.
func (w *Writer) Begin(ctx context.Context, repoName string, pusherID int64) (*Push, error) {
	repo, err := w.meta.GetRepo(ctx, repoName)
	if err != nil {
		return nil, err
	}
	// The pusher already holds write or higher, because Authorize passed.
	// Resolve whether the pusher also holds admin. The force-push guard
	// requires admin for the default branch.
	perm, err := w.meta.GetACL(ctx, repo.ID, pusherID)
	if err != nil && !errors.Is(err, meta.ErrNotFound) {
		return nil, err
	}
	lk := w.lockRepo(repoName)
	lk.Lock()
	outcome := &pushOutcome{}
	nonce, err := w.register(pushOp{
		repoID:        repo.ID,
		repoName:      repoName,
		defaultBranch: repo.DefaultBranch,
		pusherIsAdmin: perm == meta.PermAdmin,
		outcome:       outcome,
	})
	if err != nil {
		lk.Unlock()
		return nil, err
	}
	return &Push{
		Nonce:   nonce,
		outcome: outcome,
		release: func() {
			w.unregister(nonce)
			lk.Unlock()
		},
	}, nil
}

// lockRepo returns the per-repo write mutex, creating it on first use.
func (w *Writer) lockRepo(repoName string) *sync.Mutex {
	w.locksMu.Lock()
	defer w.locksMu.Unlock()
	m := w.locks[repoName]
	if m == nil {
		m = &sync.Mutex{}
		w.locks[repoName] = m
	}
	return m
}

// register mints a single-use nonce for an in-flight push and records it.
func (w *Writer) register(op pushOp) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	nonce := hex.EncodeToString(b[:])
	w.pushMu.Lock()
	w.pushes[nonce] = op
	w.pushMu.Unlock()
	return nonce, nil
}

// unregister removes a nonce. It is the deferred cleanup after the push
// completes.
func (w *Writer) unregister(nonce string) {
	w.pushMu.Lock()
	delete(w.pushes, nonce)
	w.pushMu.Unlock()
}

// take looks up and burns a nonce, so a nonce is valid for exactly one call.
func (w *Writer) take(nonce string) (pushOp, bool) {
	w.pushMu.Lock()
	defer w.pushMu.Unlock()
	op, ok := w.pushes[nonce]
	if ok {
		delete(w.pushes, nonce)
	}
	return op, ok
}

// handle is the socket handler. It does these steps:
//   - It authenticates the nonce.
//   - It PUTs the quarantined objects to the store (content before pointer).
//   - It runs the per-ref CAS in a single transaction.
//
// Any failure rejects the push. At worst, this leaves orphan objects in the
// store. It never leaves a ref that points at a missing object.
func (w *Writer) handle(req hookrpc.Request) hookrpc.Response {
	op, ok := w.take(req.Nonce)
	if !ok {
		return hookrpc.Response{Reason: "invalid or expired nonce"}
	}

	// Protect the default branch: only an admin may force-push or delete it.
	// Reject per ref before anything is uploaded. The client then sees a clean
	// refusal, not a CAS failure after the fact. No objects are wasted.
	if !op.pusherIsAdmin {
		protected := "refs/heads/" + op.defaultBranch
		for _, c := range req.Commands {
			if c.Ref == protected && c.Force {
				return hookrpc.Response{Reason: fmt.Sprintf(
					"force-push or deletion of the default branch %q requires admin", op.defaultBranch)}
			}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), casTimeout)
	defer cancel()

	// 1. Content before pointer. Make the pushed objects durable first.
	if err := w.putObjects(ctx, op.repoName, req.QuarantinePath); err != nil {
		w.logger.Error("object put failed", "repo", op.repoName, "error", err)
		return hookrpc.Response{Reason: "object upload failed"}
	}

	if w.beforeCAS != nil {
		if err := w.beforeCAS(); err != nil {
			return hookrpc.Response{Reason: err.Error()}
		}
	}

	// 2. The ref CAS. This is the linearization point. One transaction covers
	// all refs, so a multi-ref push is all-or-nothing.
	updates := make([]meta.RefUpdate, len(req.Commands))
	for i, c := range req.Commands {
		updates[i] = meta.RefUpdate{Name: c.Ref, Old: c.Old, New: c.New}
	}
	if err := w.meta.CASRefs(ctx, op.repoID, updates); err != nil {
		var mm *meta.CASMismatchError
		if errors.As(err, &mm) {
			return hookrpc.Response{Reason: "non-fast-forward (ref moved concurrently): " + mm.Error()}
		}
		w.logger.Error("ref cas failed", "repo", op.repoName, "error", err)
		return hookrpc.Response{Reason: "ref update failed"}
	}

	// The push is committed. Record what advanced, so that the HTTP handler can
	// fire the after-commit hook once receive-pack has finished (see
	// FireAfterCommit).
	commits := make([]CommitInfo, len(req.Commands))
	for i, c := range req.Commands {
		commits[i] = CommitInfo{
			RepoID:   op.repoID,
			RepoName: op.repoName,
			Ref:      c.Ref,
			Old:      c.Old,
			New:      c.New,
		}
	}
	op.outcome.record(commits)
	return hookrpc.Response{OK: true}
}

// FireAfterCommit runs the after-commit hook for a completed push.
//
// The HTTP handler calls it after receive-pack has fully finished. So the
// on-disk repo already reflects the new refs. The Materialize of a dispatch is
// then a warm no-op. It is not a ref update that races the ref update of
// receive-pack.
//
// It is fire-and-forget. The push has already committed, so a panic or an
// error must not fail the push.
//
// pusherID is the authenticated user that pushed. That user owns the CI clone
// token.
func (w *Writer) FireAfterCommit(pusherID int64, commits []CommitInfo) {
	if w.AfterCommit == nil || len(commits) == 0 {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			w.logger.Error("after-commit hook panicked", "panic", r)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), afterCommitTimeout)
	defer cancel()
	w.AfterCommit(ctx, pusherID, commits)
}

// putObjects mirrors the git objects of the quarantine into the store, under
// the prefix of the repo. It copies only true object entries (fan-out
// directories and pack/). It never copies bookkeeping files such as
// objects/info/alternates.
func (w *Writer) putObjects(ctx context.Context, repoName, quarantine string) error {
	return filepath.WalkDir(quarantine, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(quarantine, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !isObjectPath(rel) {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		return w.store.Put(ctx, repoName+"/objects/"+rel, f)
	})
}

// isObjectPath reports whether a path relative to the quarantine names a git
// object. It accepts these paths:
//   - A packfile or an index under pack/.
//   - A loose object in a fan-out directory with a two-hex name.
func isObjectPath(rel string) bool {
	head, _, ok := strings.Cut(rel, "/")
	if !ok {
		return false
	}
	if head == "pack" {
		return true
	}
	if len(head) != 2 {
		return false
	}
	for _, r := range head {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

// installHook creates a private hooks directory for core.hooksPath. The
// directory contains a `pre-receive` symlink to hookBinary.
func installHook(hookBinary string) (string, error) {
	abs, err := filepath.Abs(hookBinary)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(abs); err != nil {
		return "", fmt.Errorf("githttp: hook binary %s: %w", abs, err)
	}
	dir, err := os.MkdirTemp("", "gitmote-hooks-")
	if err != nil {
		return "", err
	}
	link := filepath.Join(dir, "pre-receive")
	if err := os.Symlink(abs, link); err != nil {
		return "", err
	}
	return dir, nil
}
