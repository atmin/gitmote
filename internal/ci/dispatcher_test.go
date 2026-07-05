package ci

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/atmin/gitmote/internal/meta"
	"github.com/atmin/gitmote/internal/repo"
	"github.com/atmin/gitmote/internal/store"
)

// newFixture wires a local meta, an in-memory store, and a materializer — the
// real collaborators the dispatcher reads workflow config through.
func newFixture(t *testing.T) (*meta.Metadata, store.Store, *repo.Materializer) {
	t.Helper()
	md, err := meta.Open(context.Background(), meta.Config{
		LocalPath: filepath.Join(t.TempDir(), "meta.sqlite3"),
	})
	if err != nil {
		t.Fatalf("meta.Open: %v", err)
	}
	t.Cleanup(func() { _ = md.Close() })
	s := store.NewMem()
	mz := repo.New(md, s, t.TempDir())
	return md, s, mz
}

// gitFixture runs a git command hermetically, failing the test on nonzero exit.
func gitFixture(t *testing.T, dir string, args ...string) string {
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

// seedRepo builds a real repo with the given files, loads its objects into the
// store the materializer reads, records the repo + branch ref in meta, and
// returns the repo row and the head SHA. An empty files map makes a repo with a
// single placeholder file (so the commit is non-empty) and no workflows.
func seedRepo(t *testing.T, md *meta.Metadata, s store.Store, name string, files map[string]string) (*meta.Repo, string) {
	t.Helper()
	ctx := context.Background()

	src := t.TempDir()
	gitFixture(t, src, "init", "-b", "main", ".")
	if len(files) == 0 {
		files = map[string]string{"README.md": "# repo\n"}
	}
	for rel, content := range files {
		full := filepath.Join(src, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitFixture(t, src, "add", "-A")
	gitFixture(t, src, "commit", "-m", "seed")
	head := gitFixture(t, src, "rev-parse", "HEAD")

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
		return s.Put(ctx, name+"/objects/"+filepath.ToSlash(rel), bytes.NewReader(data))
	})
	if err != nil {
		t.Fatalf("seed objects: %v", err)
	}

	r, err := md.CreateRepo(ctx, name, "main")
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	if err := md.CASRef(ctx, r.ID, "refs/heads/main", meta.ZeroSHA, head); err != nil {
		t.Fatalf("CASRef: %v", err)
	}
	return r, head
}

func branchEvent(r *meta.Repo, sha string) Event {
	return Event{RepoID: r.ID, RepoName: r.Name, Ref: "refs/heads/main", OldSHA: meta.ZeroSHA, NewSHA: sha}
}

// stubTrigger records the env of each trigger and can be made to fail.
type stubTrigger struct {
	calls []map[string]string
	err   error
}

func (s *stubTrigger) Trigger(_ context.Context, env map[string]string) error {
	s.calls = append(s.calls, env)
	return s.err
}

func newDispatcher(md *meta.Metadata, mz *repo.Materializer, tr Trigger) *Dispatcher {
	return NewDispatcher(Config{
		Runs:         md,
		Materializer: mz,
		Trigger:      tr,
		BaseURL:      "https://gitmote.test",
		WorkerSecret: "worker-secret",
	})
}

func TestDispatchOneWorkflowCreatesRunAndJob(t *testing.T) {
	ctx := context.Background()
	md, s, mz := newFixture(t)
	r, head := seedRepo(t, md, s, "app", map[string]string{
		".github/workflows/ci.yml": "name: CI\non: push\njobs:\n  build:\n    runs-on: ubuntu-latest\n",
	})

	tr := &stubTrigger{}
	newDispatcher(md, mz, tr).Dispatch(ctx, branchEvent(r, head))

	runs, _ := md.ListRuns(ctx, r.ID, 0)
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	if runs[0].Status != meta.RunQueued || runs[0].SHA != head {
		t.Errorf("run = %+v, want queued at %s", runs[0], head)
	}
	jobs, _ := md.ListJobs(ctx, runs[0].ID)
	if len(jobs) != 1 || jobs[0].Name != "ci.yml" || jobs[0].Status != meta.RunQueued {
		t.Errorf("jobs = %+v, want one queued job named ci.yml", jobs)
	}

	// The job was triggered exactly once, with the runner env for that run/job.
	if len(tr.calls) != 1 {
		t.Fatalf("trigger calls = %d, want 1", len(tr.calls))
	}
	env := tr.calls[0]
	if env["GITMOTE_CI_RUN_ID"] != strconv.FormatInt(runs[0].ID, 10) ||
		env["GITMOTE_CI_JOB_ID"] != strconv.FormatInt(jobs[0].ID, 10) {
		t.Errorf("env run/job ids = %q/%q, want %d/%d",
			env["GITMOTE_CI_RUN_ID"], env["GITMOTE_CI_JOB_ID"], runs[0].ID, jobs[0].ID)
	}
	if env["GITMOTE_REPO"] != "app" || env["GITMOTE_SHA"] != head ||
		env["GITMOTE_REF"] != "refs/heads/main" {
		t.Errorf("env checkout coords = %+v, want repo/sha/ref set", env)
	}
	if env["GITMOTE_URL"] != "https://gitmote.test" || env["WORKER_SECRET"] != "worker-secret" {
		t.Errorf("env url/secret = %q/%q, want configured values", env["GITMOTE_URL"], env["WORKER_SECRET"])
	}
}

func TestDispatchTwoWorkflowsCreateTwoJobs(t *testing.T) {
	ctx := context.Background()
	md, s, mz := newFixture(t)
	r, head := seedRepo(t, md, s, "app", map[string]string{
		".github/workflows/ci.yml":      "name: CI\non: push\n",
		".github/workflows/deploy.yaml": "name: Deploy\non: push\n",
	})

	newDispatcher(md, mz, &stubTrigger{}).Dispatch(ctx, branchEvent(r, head))

	runs, _ := md.ListRuns(ctx, r.ID, 0)
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	jobs, _ := md.ListJobs(ctx, runs[0].ID)
	if len(jobs) != 2 {
		t.Fatalf("jobs = %d, want 2", len(jobs))
	}
	names := map[string]bool{jobs[0].Name: true, jobs[1].Name: true}
	if !names["ci.yml"] || !names["deploy.yaml"] {
		t.Errorf("job names = %v, want ci.yml + deploy.yaml", names)
	}
}

func TestDispatchNoWorkflowsNoRun(t *testing.T) {
	ctx := context.Background()
	md, s, mz := newFixture(t)
	r, head := seedRepo(t, md, s, "app", nil) // no .github/workflows

	newDispatcher(md, mz, &stubTrigger{}).Dispatch(ctx, branchEvent(r, head))

	if runs, _ := md.ListRuns(ctx, r.ID, 0); len(runs) != 0 {
		t.Errorf("runs = %d, want 0 (no workflows)", len(runs))
	}
}

func TestDispatchMalformedWorkflowFailsRun(t *testing.T) {
	ctx := context.Background()
	md, s, mz := newFixture(t)
	r, head := seedRepo(t, md, s, "app", map[string]string{
		".github/workflows/good.yml": "name: Good\non: push\n",
		".github/workflows/bad.yml":  "name: Bad\n  : : not: valid: yaml\n\t- broken\n",
	})

	// Must not panic; the push (simulated by the event) is unaffected.
	newDispatcher(md, mz, &stubTrigger{}).Dispatch(ctx, branchEvent(r, head))

	runs, _ := md.ListRuns(ctx, r.ID, 0)
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	if runs[0].Status != meta.RunFailed {
		t.Errorf("run status = %q, want failed", runs[0].Status)
	}
	// The parsable file still got a job row, so the failure is visible in the UI.
	jobs, _ := md.ListJobs(ctx, runs[0].ID)
	if len(jobs) != 1 || jobs[0].Name != "good.yml" {
		t.Errorf("jobs = %+v, want just the parsable good.yml", jobs)
	}
}

func TestDispatchIgnoresTagsAndDeletes(t *testing.T) {
	ctx := context.Background()
	md, s, mz := newFixture(t)
	r, head := seedRepo(t, md, s, "app", map[string]string{
		".github/workflows/ci.yml": "name: CI\non: push\n",
	})
	d := newDispatcher(md, mz, &stubTrigger{})

	cases := []Event{
		{RepoID: r.ID, RepoName: r.Name, Ref: "refs/tags/v1", OldSHA: meta.ZeroSHA, NewSHA: head},
		{RepoID: r.ID, RepoName: r.Name, Ref: "refs/heads/main", OldSHA: head, NewSHA: meta.ZeroSHA},
		{RepoID: r.ID, RepoName: r.Name, Ref: "refs/heads/main", OldSHA: head, NewSHA: ""},
		{RepoID: r.ID, RepoName: r.Name, Ref: "refs/notes/commits", OldSHA: meta.ZeroSHA, NewSHA: head},
	}
	for _, ev := range cases {
		d.Dispatch(ctx, ev)
	}
	if runs, _ := md.ListRuns(ctx, r.ID, 0); len(runs) != 0 {
		t.Errorf("runs = %d, want 0 (tags/deletes/non-branch create none)", len(runs))
	}
}

func TestDispatchAllTriggersFailMarksRunError(t *testing.T) {
	ctx := context.Background()
	md, s, mz := newFixture(t)
	r, head := seedRepo(t, md, s, "app", map[string]string{
		".github/workflows/ci.yml": "name: CI\non: push\n",
	})

	tr := &stubTrigger{err: errors.New("scaleway down")}
	newDispatcher(md, mz, tr).Dispatch(ctx, branchEvent(r, head))

	runs, _ := md.ListRuns(ctx, r.ID, 0)
	if len(runs) != 1 || runs[0].Status != meta.RunError {
		t.Fatalf("run = %+v, want one run in error", runs)
	}
	jobs, _ := md.ListJobs(ctx, runs[0].ID)
	if len(jobs) != 1 || jobs[0].Status != meta.RunError {
		t.Errorf("jobs = %+v, want the job marked error", jobs)
	}
}

func TestDispatchOneTriggerFailsOthersProceed(t *testing.T) {
	ctx := context.Background()
	md, s, mz := newFixture(t)
	r, head := seedRepo(t, md, s, "app", map[string]string{
		".github/workflows/a.yml": "name: A\non: push\n",
		".github/workflows/b.yml": "name: B\non: push\n",
	})

	// Fail the first trigger, succeed the rest — one failure must not abort others.
	tr := &failNthTrigger{failOn: 1}
	newDispatcher(md, mz, tr).Dispatch(ctx, branchEvent(r, head))

	runs, _ := md.ListRuns(ctx, r.ID, 0)
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	// Not every job failed, so the run is not marked error; it stays queued.
	if runs[0].Status != meta.RunQueued {
		t.Errorf("run status = %q, want queued (not all jobs failed)", runs[0].Status)
	}
	jobs, _ := md.ListJobs(ctx, runs[0].ID)
	if len(jobs) != 2 {
		t.Fatalf("jobs = %d, want 2 (both created despite one trigger failure)", len(jobs))
	}
	var errored, queued int
	for _, j := range jobs {
		switch j.Status {
		case meta.RunError:
			errored++
		case meta.RunQueued:
			queued++
		}
	}
	if errored != 1 || queued != 1 {
		t.Errorf("job statuses = %+v, want one error + one queued", jobs)
	}
}

// stubMinter records the MintScoped call and returns a fixed token.
type stubMinter struct {
	token     string
	err       error
	userID    int64
	repoScope *int64
	readOnly  bool
	expiresAt time.Time
}

func (m *stubMinter) MintScoped(_ context.Context, userID int64, _ string, repoScope *int64, readOnly bool, expiresAt time.Time) (string, error) {
	m.userID = userID
	m.repoScope = repoScope
	m.readOnly = readOnly
	m.expiresAt = expiresAt
	return m.token, m.err
}

func TestDispatchMintsScopedCloneToken(t *testing.T) {
	ctx := context.Background()
	md, s, mz := newFixture(t)
	r, head := seedRepo(t, md, s, "app", map[string]string{
		".github/workflows/ci.yml": "name: CI\non: push\n",
	})

	tr := &stubTrigger{}
	mint := &stubMinter{token: "gmt_scoped.tok"}
	NewDispatcher(Config{
		Runs: md, Materializer: mz, Trigger: tr, Minter: mint,
		BaseURL: "https://gitmote.test", WorkerSecret: "worker-secret",
	}).Dispatch(ctx, Event{
		RepoID: r.ID, RepoName: r.Name, Ref: "refs/heads/main",
		OldSHA: meta.ZeroSHA, NewSHA: head, PusherID: 42,
	})

	if len(tr.calls) != 1 {
		t.Fatalf("trigger calls = %d, want 1", len(tr.calls))
	}
	if got := tr.calls[0]["GITMOTE_CI_CLONE_TOKEN"]; got != "gmt_scoped.tok" {
		t.Errorf("GITMOTE_CI_CLONE_TOKEN = %q, want the minted token", got)
	}
	// Minted under the pusher, read-only, scoped to just this repo, and expiring.
	if mint.userID != 42 {
		t.Errorf("mint userID = %d, want the pusher (42)", mint.userID)
	}
	if mint.repoScope == nil || *mint.repoScope != r.ID {
		t.Errorf("mint repoScope = %v, want &%d", mint.repoScope, r.ID)
	}
	if !mint.readOnly {
		t.Error("mint readOnly = false, want true — the clone token must be read-only")
	}
	if mint.expiresAt.IsZero() {
		t.Error("mint expiresAt is zero, want a bounded TTL")
	}
}

func TestDispatchMintFailureLeavesRunUsable(t *testing.T) {
	ctx := context.Background()
	md, s, mz := newFixture(t)
	r, head := seedRepo(t, md, s, "app", map[string]string{
		".github/workflows/ci.yml": "name: CI\non: push\n",
	})

	// A mint failure is non-fatal: the run and job still record and trigger, with
	// an empty clone token (the runner's clone fails visibly, not the push).
	tr := &stubTrigger{}
	mint := &stubMinter{err: errors.New("mint down")}
	NewDispatcher(Config{
		Runs: md, Materializer: mz, Trigger: tr, Minter: mint,
		BaseURL: "https://gitmote.test", WorkerSecret: "worker-secret",
	}).Dispatch(ctx, branchEvent(r, head))

	runs, _ := md.ListRuns(ctx, r.ID, 0)
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1 despite mint failure", len(runs))
	}
	if len(tr.calls) != 1 || tr.calls[0]["GITMOTE_CI_CLONE_TOKEN"] != "" {
		t.Errorf("want one trigger with an empty clone token, got %+v", tr.calls)
	}
}

// stubSecrets returns a fixed secret map (or an error) for any repo.
type stubSecrets struct {
	env map[string]string
	err error
}

func (s stubSecrets) Secrets(context.Context, int64) (map[string]string, error) {
	return s.env, s.err
}

func TestDispatchInjectsSecrets(t *testing.T) {
	ctx := context.Background()
	md, s, mz := newFixture(t)
	r, head := seedRepo(t, md, s, "app", map[string]string{
		".github/workflows/ci.yml": "name: CI\non: push\n",
	})

	tr := &stubTrigger{}
	NewDispatcher(Config{
		Runs: md, Materializer: mz, Trigger: tr,
		Secrets:      stubSecrets{env: map[string]string{"API_TOKEN": "s3cr3t"}},
		BaseURL:      "https://gitmote.test",
		WorkerSecret: "worker-secret",
	}).Dispatch(ctx, branchEvent(r, head))

	if len(tr.calls) != 1 {
		t.Fatalf("trigger calls = %d, want 1", len(tr.calls))
	}
	env := tr.calls[0]
	// Secrets are namespaced so the engine can tell them from coordinates.
	if env["GITMOTE_CI_SECRET_API_TOKEN"] != "s3cr3t" {
		t.Errorf("GITMOTE_CI_SECRET_API_TOKEN = %q, want the injected value", env["GITMOTE_CI_SECRET_API_TOKEN"])
	}
	// The namespacing keeps a secret from ever shadowing the runner's coordinates.
	if env["GITMOTE_URL"] != "https://gitmote.test" || env["GITMOTE_REPO"] != "app" {
		t.Errorf("coordinates altered by secret injection: %+v", env)
	}
}

func TestDispatchSecretsFailureRunsWithout(t *testing.T) {
	ctx := context.Background()
	md, s, mz := newFixture(t)
	r, head := seedRepo(t, md, s, "app", map[string]string{
		".github/workflows/ci.yml": "name: CI\non: push\n",
	})

	// A secrets error is non-fatal: the job still triggers, just without secrets.
	tr := &stubTrigger{}
	NewDispatcher(Config{
		Runs: md, Materializer: mz, Trigger: tr,
		Secrets:      stubSecrets{err: errors.New("keyring down")},
		BaseURL:      "https://gitmote.test",
		WorkerSecret: "worker-secret",
	}).Dispatch(ctx, branchEvent(r, head))

	if len(tr.calls) != 1 {
		t.Fatalf("trigger calls = %d, want 1 (secrets failure must not abort dispatch)", len(tr.calls))
	}
	if tr.calls[0]["GITMOTE_REPO"] != "app" {
		t.Errorf("job env missing fixed coords: %+v", tr.calls[0])
	}
}

// failNthTrigger fails the Nth (1-based) trigger call and succeeds the others.
type failNthTrigger struct {
	failOn int
	n      int
}

func (f *failNthTrigger) Trigger(_ context.Context, _ map[string]string) error {
	f.n++
	if f.n == f.failOn {
		return errors.New("trigger failed")
	}
	return nil
}
