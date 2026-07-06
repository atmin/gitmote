package githttp

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/atmin/gitmote/internal/auth"
	"github.com/atmin/gitmote/internal/ci"
	"github.com/atmin/gitmote/internal/meta"
	"github.com/atmin/gitmote/internal/repo"
	"github.com/atmin/gitmote/internal/store"
)

var (
	hookOnce sync.Once
	hookPath string
	hookErr  error
)

// buildHook compiles the pre-receive hook binary once per test run and returns
// its path.
func buildHook(t *testing.T) string {
	t.Helper()
	hookOnce.Do(func() {
		dir, err := os.MkdirTemp("", "gitmote-hookbin-")
		if err != nil {
			hookErr = err
			return
		}
		out := filepath.Join(dir, "gitmote-hook")
		cmd := exec.Command("go", "build", "-o", out, "github.com/atmin/gitmote/cmd/gitmote-hook")
		if b, err := cmd.CombinedOutput(); err != nil {
			hookErr = fmt.Errorf("build hook: %v\n%s", err, b)
			return
		}
		hookPath = out
	})
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	return hookPath
}

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

// newWriteServer wires meta + mem store + materializer + write coordinator
// behind a guarded handler on an httptest server.
func newWriteServer(t *testing.T) (*httptest.Server, *meta.Metadata, store.Store, *Writer) {
	t.Helper()
	m := openMeta(t)
	s := store.NewMem()
	// A short socket dir: unix socket paths are limited to ~104 bytes, and
	// t.TempDir() embeds the (long) test name.
	sockDir, err := os.MkdirTemp("", "gm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "s.sock")
	writer, err := NewWriter(m, s, buildHook(t), sock, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	t.Cleanup(func() { _ = writer.Close() })

	h, err := New(Config{
		Materializer: repo.New(m, s, t.TempDir()),
		Authorizer:   auth.NewGuard(m),
		Writer:       writer,
		Logger:       slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("githttp.New: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, m, s, writer
}

// newGatedServer is newWriteServer with a controllable writer-lease gate
// (backend Config.IsWritable), to exercise a follower's push refusal.
func newGatedServer(t *testing.T, isWritable func() bool) (*httptest.Server, *meta.Metadata, store.Store) {
	t.Helper()
	m := openMeta(t)
	s := store.NewMem()
	sockDir, err := os.MkdirTemp("", "gm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	writer, err := NewWriter(m, s, buildHook(t), filepath.Join(sockDir, "s.sock"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	h, err := New(Config{
		Materializer: repo.New(m, s, t.TempDir()),
		Authorizer:   auth.NewGuard(m),
		Writer:       writer,
		IsWritable:   isWritable,
		Logger:       slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("githttp.New: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, m, s
}

// TestWriteGateFollowerRefusesPush exercises the writer-lease gate: a follower
// (IsWritable false) refuses receive-pack while still serving reads; promoting
// back to leader accepts the same push. Mirrors the deploy-handoff window where
// a new instance is up read-only before it acquires the lease (safety.md §1).
func TestWriteGateFollowerRefusesPush(t *testing.T) {
	ctx := context.Background()
	leader := true
	srv, m, _ := newGatedServer(t, func() bool { return leader })

	r, err := m.CreateRepo(ctx, repoName, "main")
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	raw := mintUser(t, m, r.ID, "atmin", meta.PermWrite)
	remote := authedURL(srv.URL, raw) + "/" + repoName

	// Golden path: the leader accepts the first push.
	src := initCommit(t, "hello\n")
	head1 := git(t, src, "rev-parse", "HEAD")
	git(t, src, "push", remote, "main")
	if got := serverRefSHA(t, m, r.ID, "refs/heads/main"); got != head1 {
		t.Fatalf("after leader push, server main = %q, want %q", got, head1)
	}

	// Become a follower: reads still serve, pushes are refused, ref unchanged.
	leader = false
	dst := filepath.Join(t.TempDir(), "clone")
	git(t, "", "clone", remote, dst) // a read succeeds on a follower
	if got := git(t, dst, "rev-parse", "HEAD"); got != head1 {
		t.Errorf("follower clone HEAD = %q, want %q", got, head1)
	}

	if err := os.WriteFile(filepath.Join(src, "file.txt"), []byte("hello\nagain\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, src, "commit", "-am", "second")
	head2 := git(t, src, "rev-parse", "HEAD")
	out, err := tryGit(src, "push", remote, "main")
	if err == nil {
		t.Fatalf("follower push succeeded, want rejection\n%s", out)
	}
	if !strings.Contains(out, "503") {
		t.Errorf("follower push error missing 503 signal:\n%s", out)
	}
	if got := serverRefSHA(t, m, r.ID, "refs/heads/main"); got != head1 {
		t.Errorf("follower push advanced ref to %q, want unchanged %q", got, head1)
	}

	// Promote back to leader: the same push now succeeds.
	leader = true
	git(t, src, "push", remote, "main")
	if got := serverRefSHA(t, m, r.ID, "refs/heads/main"); got != head2 {
		t.Errorf("after promotion, server main = %q, want %q", got, head2)
	}
}

// TestPushChunkedLargePack pushes a pack larger than git's default
// http.postBuffer (1 MiB) with the client at its defaults, so git sends the body
// with chunked transfer-encoding (no Content-Length). The server must buffer it
// to a length before the CGI hand-off; without that, git http-backend 400s.
func TestPushChunkedLargePack(t *testing.T) {
	ctx := context.Background()
	srv, m, s, _ := newWriteServer(t)

	r, err := m.CreateRepo(ctx, repoName, "main")
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	raw := mintUser(t, m, r.ID, "atmin", meta.PermWrite)
	remote := authedURL(srv.URL, raw) + "/" + repoName

	// A 3 MiB incompressible blob → a pack well over the 1 MiB postBuffer, so the
	// client streams it chunked (the harness git uses default config: no override).
	src := t.TempDir()
	git(t, src, "init", "-b", "main", ".")
	big := make([]byte, 3<<20)
	if _, err := rand.Read(big); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "big.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, src, "add", ".")
	git(t, src, "commit", "-m", "big")
	head := git(t, src, "rev-parse", "HEAD")

	git(t, src, "push", remote, "main") // fails (HTTP 400) before the server-side buffering

	if got := serverRefSHA(t, m, r.ID, "refs/heads/main"); got != head {
		t.Fatalf("server main = %q, want %q", got, head)
	}
	objs, _ := s.List(ctx, repoName+"/objects/")
	if len(objs) == 0 {
		t.Fatal("no objects uploaded after chunked push")
	}
}

// noopTrigger is a ci.Trigger that does nothing — the write-path tests only care
// that runs/jobs are recorded, not that a runner starts.
type noopTrigger struct{}

func (noopTrigger) Trigger(context.Context, map[string]string) error { return nil }

// wireDispatcher points the writer's after-commit hook at a real ci.Dispatcher
// reading workflow config through a materializer over store s, mirroring the
// main.go wiring.
func wireDispatcher(t *testing.T, w *Writer, m *meta.Metadata, s store.Store) {
	t.Helper()
	d := ci.NewDispatcher(ci.Config{
		Runs:         m,
		Materializer: repo.New(m, s, t.TempDir()),
		Trigger:      noopTrigger{},
		Logger:       slog.New(slog.DiscardHandler),
	})
	w.AfterCommit = func(ctx context.Context, pusherID int64, commits []CommitInfo) {
		for _, c := range commits {
			d.Dispatch(ctx, ci.Event{
				RepoID:   c.RepoID,
				RepoName: c.RepoName,
				Ref:      c.Ref,
				OldSHA:   c.Old,
				NewSHA:   c.New,
				PusherID: pusherID,
			})
		}
	}
}

// initWorkflowCommit makes a source repo whose first commit carries a valid
// workflow, so a push through the dispatcher discovers work and enqueues a run.
func initWorkflowCommit(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	git(t, src, "init", "-b", "main", ".")
	if err := os.WriteFile(filepath.Join(src, "file.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, ".gitmote", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, ".gitmote", "workflows", "ci.yml"),
		[]byte("name: CI\non: push\njobs:\n  build:\n    runs-on: ubuntu-latest\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, src, "add", "-A")
	git(t, src, "commit", "-m", "commit")
	return src
}

// TestAfterCommitEnqueuesRun drives a real git push of a repo with a workflow
// and asserts the after-commit seam discovers it and records one queued run and
// job for the advanced branch tip.
func TestAfterCommitEnqueuesRun(t *testing.T) {
	ctx := context.Background()
	srv, m, s, writer := newWriteServer(t)
	wireDispatcher(t, writer, m, s)

	r, err := m.CreateRepo(ctx, repoName, "main")
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	raw := mintUser(t, m, r.ID, "atmin", meta.PermWrite)
	remote := authedURL(srv.URL, raw) + "/" + repoName

	src := initWorkflowCommit(t)
	head := git(t, src, "rev-parse", "HEAD")
	git(t, src, "push", remote, "main")

	runs, err := m.ListRuns(ctx, r.ID, 0)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("after push, runs = %d, want exactly 1", len(runs))
	}
	if runs[0].Ref != "refs/heads/main" || runs[0].SHA != head || runs[0].Status != meta.RunQueued {
		t.Errorf("run = %+v, want ref refs/heads/main sha %q queued", runs[0], head)
	}
	jobs, _ := m.ListJobs(ctx, runs[0].ID)
	if len(jobs) != 1 || jobs[0].Name != "ci.yml" {
		t.Errorf("jobs = %+v, want one job named ci.yml", jobs)
	}
}

// TestAfterCommitNoWorkflowNoRun confirms the discovery gate end-to-end: a real
// push of a repo without .gitmote/workflows records no run.
func TestAfterCommitNoWorkflowNoRun(t *testing.T) {
	ctx := context.Background()
	srv, m, s, writer := newWriteServer(t)
	wireDispatcher(t, writer, m, s)

	r, _ := m.CreateRepo(ctx, repoName, "main")
	raw := mintUser(t, m, r.ID, "atmin", meta.PermWrite)
	remote := authedURL(srv.URL, raw) + "/" + repoName

	src := initCommit(t, "hello\n") // no workflow file
	git(t, src, "push", remote, "main")

	if runs, _ := m.ListRuns(ctx, r.ID, 0); len(runs) != 0 {
		t.Errorf("runs = %d, want 0 (no workflows)", len(runs))
	}
}

// TestRejectedPushEnqueuesNoRun asserts a non-fast-forward push — refused at the
// CAS — never fires the after-commit seam, so no run is recorded for it.
func TestRejectedPushEnqueuesNoRun(t *testing.T) {
	ctx := context.Background()
	srv, m, s, writer := newWriteServer(t)
	wireDispatcher(t, writer, m, s)

	r, _ := m.CreateRepo(ctx, repoName, "main")
	raw := mintUser(t, m, r.ID, "atmin", meta.PermWrite)
	remote := authedURL(srv.URL, raw) + "/" + repoName

	// Establish main with a workflow (one accepted push → one run).
	src := initWorkflowCommit(t)
	git(t, src, "push", remote, "main")

	// A stale clone diverges and pushes a non-fast-forward, which is rejected.
	stale := filepath.Join(t.TempDir(), "stale")
	git(t, "", "clone", remote, stale)
	if err := os.WriteFile(filepath.Join(src, "file.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, src, "commit", "-am", "advance")
	git(t, src, "push", remote, "main")

	if err := os.WriteFile(filepath.Join(stale, "file.txt"), []byte("one\ndivergent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, stale, "commit", "-am", "divergent")
	if out, err := tryGit(stale, "push", remote, "main"); err == nil {
		t.Fatalf("stale push succeeded, want rejection\n%s", out)
	}

	// Two accepted pushes → two runs; the rejected push adds none.
	runs, err := m.ListRuns(ctx, r.ID, 0)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Errorf("runs = %d, want 2 (rejected push enqueues none)", len(runs))
	}
}

// TestAfterCommitPanicLeavesPushGreen is the safety invariant: an after-commit
// hook that panics must not fail the push — the ref still advances and the
// client sees a green push (content-before-pointer applied to CI).
func TestAfterCommitPanicLeavesPushGreen(t *testing.T) {
	ctx := context.Background()
	srv, m, _, writer := newWriteServer(t)
	writer.AfterCommit = func(context.Context, int64, []CommitInfo) {
		panic("dispatch blew up")
	}

	r, _ := m.CreateRepo(ctx, repoName, "main")
	raw := mintUser(t, m, r.ID, "atmin", meta.PermWrite)
	remote := authedURL(srv.URL, raw) + "/" + repoName

	src := initCommit(t, "hello\n")
	head := git(t, src, "rev-parse", "HEAD")
	if out, err := tryGit(src, "push", remote, "main"); err != nil {
		t.Fatalf("push failed despite panicking after-commit hook: %v\n%s", err, out)
	}
	if got := serverRefSHA(t, m, r.ID, "refs/heads/main"); got != head {
		t.Errorf("main = %q, want %q — ref must advance even when the hook panics", got, head)
	}
}

// tryGit runs a git command hermetically and returns its combined output and
// error without failing the test — for operations expected to be rejected.
func tryGit(dir string, args ...string) (string, error) {
	// Disable any credential helper so a token from an earlier authed push is not
	// cached and reused, keeping an "anonymous" request genuinely anonymous.
	cmd := exec.CommandContext(context.Background(), "git", append([]string{"-c", "credential.helper="}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// initCommit makes a fresh source repo with one commit on main and returns its
// dir and head sha.
func initCommit(t *testing.T, content string) string {
	t.Helper()
	src := t.TempDir()
	git(t, src, "init", "-b", "main", ".")
	if err := os.WriteFile(filepath.Join(src, "file.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, src, "add", ".")
	git(t, src, "commit", "-m", "commit")
	return src
}

// serverRefSHA returns the sha meta holds for a ref, or "" if absent.
func serverRefSHA(t *testing.T, m *meta.Metadata, repoID int64, name string) string {
	t.Helper()
	refs, err := m.ListRefs(context.Background(), repoID)
	if err != nil {
		t.Fatalf("ListRefs: %v", err)
	}
	for _, r := range refs {
		if r.Name == name {
			return r.SHA
		}
	}
	return ""
}

func TestPushThenClone(t *testing.T) {
	ctx := context.Background()
	srv, m, s, _ := newWriteServer(t)

	r, err := m.CreateRepo(ctx, repoName, "main")
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	raw := mintUser(t, m, r.ID, "atmin", meta.PermWrite)
	remote := authedURL(srv.URL, raw) + "/" + repoName

	// Golden path: push a new repo's first commits.
	src := initCommit(t, "hello\n")
	head := git(t, src, "rev-parse", "HEAD")
	git(t, src, "push", remote, "main")

	// The ref advanced in s3lite and the objects are durable in the store.
	if got := serverRefSHA(t, m, r.ID, "refs/heads/main"); got != head {
		t.Fatalf("server main = %q, want %q", got, head)
	}
	objs, _ := s.List(ctx, repoName+"/objects/")
	if len(objs) == 0 {
		t.Fatal("no objects uploaded to the store after push")
	}

	// A clone returns exactly what was pushed, and fsck is clean.
	dst := filepath.Join(t.TempDir(), "clone")
	git(t, "", "clone", remote, dst)
	if got := git(t, dst, "rev-parse", "HEAD"); got != head {
		t.Errorf("cloned HEAD = %q, want %q", got, head)
	}
	if data, _ := os.ReadFile(filepath.Join(dst, "file.txt")); string(data) != "hello\n" {
		t.Errorf("cloned file = %q, want \"hello\\n\"", data)
	}
	git(t, dst, "fsck", "--strict")

	// A follow-up fast-forward push is accepted.
	if err := os.WriteFile(filepath.Join(src, "file.txt"), []byte("hello\nagain\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, src, "commit", "-am", "second")
	head2 := git(t, src, "rev-parse", "HEAD")
	git(t, src, "push", remote, "main")
	if got := serverRefSHA(t, m, r.ID, "refs/heads/main"); got != head2 {
		t.Errorf("server main after ff = %q, want %q", got, head2)
	}
}

func TestNonFastForwardRejected(t *testing.T) {
	ctx := context.Background()
	srv, m, s, _ := newWriteServer(t)
	r, _ := m.CreateRepo(ctx, repoName, "main")
	raw := mintUser(t, m, r.ID, "atmin", meta.PermWrite)
	remote := authedURL(srv.URL, raw) + "/" + repoName

	// Establish main.
	src := initCommit(t, "one\n")
	git(t, src, "push", remote, "main")
	base := git(t, src, "rev-parse", "HEAD")

	// A stale clone that still sees main at base.
	stale := filepath.Join(t.TempDir(), "stale")
	git(t, "", "clone", remote, stale)

	// Meanwhile main advances on the server.
	if err := os.WriteFile(filepath.Join(src, "file.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, src, "commit", "-am", "advance")
	git(t, src, "push", remote, "main")
	advanced := git(t, src, "rev-parse", "HEAD")

	// The stale client commits on top of base and pushes — a non-fast-forward
	// against the advanced tip. It must be rejected, main unchanged.
	if err := os.WriteFile(filepath.Join(stale, "file.txt"), []byte("one\ndivergent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, stale, "commit", "-am", "divergent")
	if out, err := tryGit(stale, "push", remote, "main"); err == nil {
		t.Fatalf("stale push succeeded, want rejection\n%s", out)
	}
	if got := serverRefSHA(t, m, r.ID, "refs/heads/main"); got != advanced {
		t.Errorf("main after rejected push = %q, want %q (unchanged)", got, advanced)
	}
	_ = base
	_ = s
}

func TestConcurrentPushOneWins(t *testing.T) {
	ctx := context.Background()
	srv, m, _, _ := newWriteServer(t)
	r, _ := m.CreateRepo(ctx, repoName, "main")
	raw := mintUser(t, m, r.ID, "atmin", meta.PermWrite)
	remote := authedURL(srv.URL, raw) + "/" + repoName

	// A base commit both racers build on.
	src := initCommit(t, "base\n")
	git(t, src, "push", remote, "main")

	// Two independent clones, each with its own new commit on top of base.
	dirs := [2]string{filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")}
	heads := [2]string{}
	for i, d := range dirs {
		git(t, "", "clone", remote, d)
		if err := os.WriteFile(filepath.Join(d, "file.txt"), []byte(fmt.Sprintf("base\nracer-%d\n", i)), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, d, "commit", "-am", "racer")
		heads[i] = git(t, d, "rev-parse", "HEAD")
	}

	// Push both concurrently; exactly one must win.
	var wg sync.WaitGroup
	errs := [2]error{}
	for i, d := range dirs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = tryGit(d, "push", remote, "main")
		}()
	}
	wg.Wait()

	wins := 0
	winner := -1
	for i := range errs {
		if errs[i] == nil {
			wins++
			winner = i
		}
	}
	if wins != 1 {
		t.Fatalf("concurrent pushes: %d winners, want exactly 1 (errs: %v)", wins, errs)
	}
	if got := serverRefSHA(t, m, r.ID, "refs/heads/main"); got != heads[winner] {
		t.Errorf("main = %q, want winner %q — no lost update", got, heads[winner])
	}
}

func TestAtomicMultiRefRollback(t *testing.T) {
	ctx := context.Background()
	srv, m, _, _ := newWriteServer(t)
	r, _ := m.CreateRepo(ctx, repoName, "main")
	raw := mintUser(t, m, r.ID, "atmin", meta.PermWrite)
	remote := authedURL(srv.URL, raw) + "/" + repoName

	src := initCommit(t, "one\n")
	git(t, src, "push", remote, "main")

	// Stale clone still at the original tip.
	stale := filepath.Join(t.TempDir(), "stale")
	git(t, "", "clone", remote, stale)

	// Advance main on the server.
	if err := os.WriteFile(filepath.Join(src, "file.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, src, "commit", "-am", "advance")
	git(t, src, "push", remote, "main")
	advanced := git(t, src, "rev-parse", "HEAD")

	// The stale client builds a new commit and a brand-new branch, then pushes
	// both atomically. The main update is a non-fast-forward (fails); with
	// --atomic the feature create must roll back too.
	if err := os.WriteFile(filepath.Join(stale, "file.txt"), []byte("one\ndivergent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, stale, "commit", "-am", "divergent")
	git(t, stale, "branch", "feature")
	if out, err := tryGit(stale, "push", "--atomic", remote, "main", "feature"); err == nil {
		t.Fatalf("atomic push succeeded, want rejection\n%s", out)
	}

	if got := serverRefSHA(t, m, r.ID, "refs/heads/main"); got != advanced {
		t.Errorf("main = %q, want %q (unchanged)", got, advanced)
	}
	if got := serverRefSHA(t, m, r.ID, "refs/heads/feature"); got != "" {
		t.Errorf("feature = %q, want absent — atomic rollback", got)
	}
}

// TestDefaultBranchForcePushGuard is the admin-only default-branch protection: a
// write collaborator may fast-forward the default branch and force-push a
// non-default branch, but a non-fast-forward of the default branch is refused;
// an admin may force-push it. (Golden + failure both, per CONTRIBUTING.)
func TestDefaultBranchForcePushGuard(t *testing.T) {
	ctx := context.Background()
	srv, m, _, _ := newWriteServer(t)
	r, _ := m.CreateRepo(ctx, repoName, "main")
	writeRaw := mintUser(t, m, r.ID, "collab", meta.PermWrite)
	adminRaw := mintUser(t, m, r.ID, "boss", meta.PermAdmin)
	writeRemote := authedURL(srv.URL, writeRaw) + "/" + repoName
	adminRemote := authedURL(srv.URL, adminRaw) + "/" + repoName

	// The collaborator establishes main (a create) and fast-forwards it — allowed.
	src := initCommit(t, "one\n")
	base := git(t, src, "rev-parse", "HEAD")
	git(t, src, "push", writeRemote, "main")
	if err := os.WriteFile(filepath.Join(src, "file.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, src, "commit", "-am", "ff")
	git(t, src, "push", writeRemote, "main")
	ff := git(t, src, "rev-parse", "HEAD")

	// The collaborator rewrites history off base and force-pushes the DEFAULT
	// branch — refused; main stays at the fast-forward tip.
	git(t, src, "reset", "--hard", base)
	if err := os.WriteFile(filepath.Join(src, "file.txt"), []byte("one\nrewritten\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, src, "commit", "-am", "rewrite")
	rewritten := git(t, src, "rev-parse", "HEAD")
	if out, err := tryGit(src, "push", "--force", writeRemote, "main"); err == nil {
		t.Fatalf("collaborator force-push of the default branch succeeded, want refusal\n%s", out)
	}
	if got := serverRefSHA(t, m, r.ID, "refs/heads/main"); got != ff {
		t.Errorf("main after refused force = %q, want unchanged %q", got, ff)
	}

	// The same rewrite is fine on a NON-default branch: create feature at the
	// fast-forward tip, then force-push the rewrite over it — allowed.
	git(t, src, "push", writeRemote, ff+":refs/heads/feature")
	if out, err := tryGit(src, "push", "--force", writeRemote, "HEAD:refs/heads/feature"); err != nil {
		t.Fatalf("collaborator force-push of a non-default branch failed, want ok: %v\n%s", err, out)
	}
	if got := serverRefSHA(t, m, r.ID, "refs/heads/feature"); got != rewritten {
		t.Errorf("feature after force = %q, want %q", got, rewritten)
	}

	// An admin may force-push the default branch.
	if out, err := tryGit(src, "push", "--force", adminRemote, "main"); err != nil {
		t.Fatalf("admin force-push of the default branch failed, want ok: %v\n%s", err, out)
	}
	if got := serverRefSHA(t, m, r.ID, "refs/heads/main"); got != rewritten {
		t.Errorf("main after admin force = %q, want %q", got, rewritten)
	}
}

// TestPublicRepoAnonymousReadWriteRefused proves visibility on the transport: a
// public repo clones with no credentials, but an anonymous push is refused.
func TestPublicRepoAnonymousReadWriteRefused(t *testing.T) {
	ctx := context.Background()
	srv, m, _, _ := newWriteServer(t)
	r, _ := m.CreateRepo(ctx, repoName, "main")
	if err := m.SetVisibility(ctx, r.ID, meta.VisibilityPublic); err != nil {
		t.Fatalf("SetVisibility: %v", err)
	}
	writeRaw := mintUser(t, m, r.ID, "collab", meta.PermWrite)

	// A collaborator seeds main.
	src := initCommit(t, "hello\n")
	git(t, src, "push", authedURL(srv.URL, writeRaw)+"/"+repoName, "main")
	head := git(t, src, "rev-parse", "HEAD")

	// Anonymous clone (no credentials) succeeds and matches.
	dst := filepath.Join(t.TempDir(), "clone")
	git(t, "", "clone", srv.URL+"/"+repoName, dst)
	if got := git(t, dst, "rev-parse", "HEAD"); got != head {
		t.Errorf("anon clone HEAD = %q, want %q", got, head)
	}

	// Anonymous push is refused, main unchanged.
	if err := os.WriteFile(filepath.Join(src, "file.txt"), []byte("hello\nmore\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, src, "commit", "-am", "next")
	if out, err := tryGit(src, "push", srv.URL+"/"+repoName, "main"); err == nil {
		t.Fatalf("anonymous push to a public repo succeeded, want refusal\n%s", out)
	}
	if got := serverRefSHA(t, m, r.ID, "refs/heads/main"); got != head {
		t.Errorf("main after refused anon push = %q, want unchanged %q", got, head)
	}
}

// TestPrivateRepoSpectatorCannotPush proves the spectator (read ACL) can clone a
// private repo but not push it.
func TestPrivateRepoSpectatorCannotPush(t *testing.T) {
	ctx := context.Background()
	srv, m, _, _ := newWriteServer(t)
	r, _ := m.CreateRepo(ctx, repoName, "main") // private by default
	writeRaw := mintUser(t, m, r.ID, "collab", meta.PermWrite)
	readRaw := mintUser(t, m, r.ID, "spectator", meta.PermRead)

	src := initCommit(t, "hello\n")
	git(t, src, "push", authedURL(srv.URL, writeRaw)+"/"+repoName, "main")
	head := git(t, src, "rev-parse", "HEAD")

	// The spectator can clone (read ACL on a private repo).
	dst := filepath.Join(t.TempDir(), "clone")
	git(t, "", "clone", authedURL(srv.URL, readRaw)+"/"+repoName, dst)

	// But cannot push.
	if err := os.WriteFile(filepath.Join(dst, "file.txt"), []byte("hello\nedit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dst, "commit", "-am", "spectator edit")
	if out, err := tryGit(dst, "push", authedURL(srv.URL, readRaw)+"/"+repoName, "main"); err == nil {
		t.Fatalf("spectator push succeeded, want refusal\n%s", out)
	}
	if got := serverRefSHA(t, m, r.ID, "refs/heads/main"); got != head {
		t.Errorf("main after refused spectator push = %q, want unchanged %q", got, head)
	}
}

func TestOrderingInvariantObjectsBeforeRef(t *testing.T) {
	ctx := context.Background()
	srv, m, s, writer := newWriteServer(t)
	r, _ := m.CreateRepo(ctx, repoName, "main")
	raw := mintUser(t, m, r.ID, "atmin", meta.PermWrite)
	remote := authedURL(srv.URL, raw) + "/" + repoName

	// Inject a CAS failure that fires *after* the object PUT. This models a ref
	// CAS that loses the race (or any post-PUT failure): objects are already
	// durable, and the ref must never advance.
	writer.beforeCAS = func() error { return errors.New("injected cas failure") }

	src := initCommit(t, "hello\n")
	if out, err := tryGit(src, "push", remote, "main"); err == nil {
		t.Fatalf("push succeeded despite injected CAS failure\n%s", out)
	}

	// Ordering invariant: the objects reached the store (harmless orphans that
	// gc reclaims), but the ref never advanced (no corruption).
	objs, _ := s.List(ctx, repoName+"/objects/")
	if len(objs) == 0 {
		t.Error("expected orphan objects in the store (content-before-pointer)")
	}
	if got := serverRefSHA(t, m, r.ID, "refs/heads/main"); got != "" {
		t.Errorf("main = %q, want absent — ref must not advance when CAS fails", got)
	}
}
