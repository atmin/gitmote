package ci

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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

func TestDispatchOneWorkflowCreatesRunAndJob(t *testing.T) {
	ctx := context.Background()
	md, s, mz := newFixture(t)
	r, head := seedRepo(t, md, s, "atmin/app", map[string]string{
		".github/workflows/ci.yml": "name: CI\non: push\njobs:\n  build:\n    runs-on: ubuntu-latest\n",
	})

	NewDispatcher(md, mz, nil).Dispatch(ctx, branchEvent(r, head))

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
}

func TestDispatchTwoWorkflowsCreateTwoJobs(t *testing.T) {
	ctx := context.Background()
	md, s, mz := newFixture(t)
	r, head := seedRepo(t, md, s, "atmin/app", map[string]string{
		".github/workflows/ci.yml":      "name: CI\non: push\n",
		".github/workflows/deploy.yaml": "name: Deploy\non: push\n",
	})

	NewDispatcher(md, mz, nil).Dispatch(ctx, branchEvent(r, head))

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
	r, head := seedRepo(t, md, s, "atmin/app", nil) // no .github/workflows

	NewDispatcher(md, mz, nil).Dispatch(ctx, branchEvent(r, head))

	if runs, _ := md.ListRuns(ctx, r.ID, 0); len(runs) != 0 {
		t.Errorf("runs = %d, want 0 (no workflows)", len(runs))
	}
}

func TestDispatchMalformedWorkflowFailsRun(t *testing.T) {
	ctx := context.Background()
	md, s, mz := newFixture(t)
	r, head := seedRepo(t, md, s, "atmin/app", map[string]string{
		".github/workflows/good.yml": "name: Good\non: push\n",
		".github/workflows/bad.yml":  "name: Bad\n  : : not: valid: yaml\n\t- broken\n",
	})

	// Must not panic; the push (simulated by the event) is unaffected.
	NewDispatcher(md, mz, nil).Dispatch(ctx, branchEvent(r, head))

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
	r, head := seedRepo(t, md, s, "atmin/app", map[string]string{
		".github/workflows/ci.yml": "name: CI\non: push\n",
	})
	d := NewDispatcher(md, mz, nil)

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
