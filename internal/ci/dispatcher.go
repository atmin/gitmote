// Package ci is the continuous-integration dispatch seam. On a successful push
// the write path hands each updated ref to Dispatcher.Dispatch, which reads the
// pushed commit's .github/workflows/ directory and — when there is work —
// records a run with one job per workflow file. It is fire-and-forget: dispatch
// must never block or fail a push (a missed run is a missed deploy, not a failed
// push — the content-before-pointer discipline applied to CI; see
// docs/architecture/safety.md and tasks/16-ci.md). Later stages add the Scaleway
// trigger.
package ci

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/atmin/gitmote/internal/meta"
	"github.com/atmin/gitmote/internal/repo"
	"gopkg.in/yaml.v3"
)

const (
	// branchPrefix is the ref namespace CI reacts to; tags and other refs are
	// ignored.
	branchPrefix = "refs/heads/"
	// workflowDir is the fixed, traversal-safe location engine (act) reads.
	workflowDir = ".github/workflows"
)

// Runs is the slice of meta the dispatcher writes through. *meta.Metadata
// satisfies it.
type Runs interface {
	CreateRun(ctx context.Context, repoID int64, ref, sha string) (*meta.Run, error)
	SetRunStatus(ctx context.Context, runID int64, status meta.RunStatus) error
	CreateJob(ctx context.Context, runID int64, name string) (*meta.Job, error)
}

// Materializer turns a repo name into an on-disk bare repo the browse reader can
// query. *repo.Materializer satisfies it.
type Materializer interface {
	Materialize(ctx context.Context, name string) (string, error)
}

// Event is one updated ref from a completed push.
type Event struct {
	RepoID   int64
	RepoName string
	Ref      string
	OldSHA   string
	NewSHA   string
}

// Dispatcher enqueues CI runs for ref advances. Only the leader constructs and
// calls it (it is the only instance that processes receive-pack).
type Dispatcher struct {
	runs   Runs
	mz     Materializer
	logger *slog.Logger
}

// NewDispatcher returns a Dispatcher writing runs through runs and reading
// workflow config via mz.
func NewDispatcher(runs Runs, mz Materializer, logger *slog.Logger) *Dispatcher {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Dispatcher{runs: runs, mz: mz, logger: logger}
}

// Dispatch reads the pushed commit's workflows and records a run for a branch
// create/update that has any. Tag pushes and branch deletions do nothing; a repo
// with no .github/workflows creates no run. Everything here is non-fatal: the
// push has already committed, so errors are logged, never returned.
func (d *Dispatcher) Dispatch(ctx context.Context, ev Event) {
	if !strings.HasPrefix(ev.Ref, branchPrefix) {
		return // not a branch — no CI
	}
	if isAbsent(ev.NewSHA) {
		return // a branch deletion — nothing to run
	}

	dir, err := d.mz.Materialize(ctx, ev.RepoName)
	if err != nil {
		// An unexpected materialize error: record a visible errored run rather
		// than silently dropping the push's CI.
		d.logger.Error("ci: materialize failed",
			"repo", ev.RepoName, "ref", ev.Ref, "sha", ev.NewSHA, "error", err)
		d.recordErrorRun(ctx, ev)
		return
	}

	entries, err := repo.Tree(ctx, dir, ev.NewSHA, workflowDir)
	if errors.Is(err, repo.ErrNotFound) {
		d.logger.Debug("ci: no workflows", "repo", ev.RepoName, "ref", ev.Ref)
		return // no .github/workflows → no run
	}
	if err != nil {
		d.logger.Error("ci: read workflows failed",
			"repo", ev.RepoName, "ref", ev.Ref, "sha", ev.NewSHA, "error", err)
		d.recordErrorRun(ctx, ev)
		return
	}

	var files []repo.TreeEntry
	for _, e := range entries {
		if e.Type == "blob" && isYAML(e.Name) {
			files = append(files, e)
		}
	}
	if len(files) == 0 {
		return // a workflows dir with no *.yml|*.yaml → nothing to run
	}

	d.recordRun(ctx, ev, dir, files)
}

// recordRun creates a queued run and one job per workflow file. A file that
// fails to parse as YAML downgrades the run to failed (the first parse error is
// logged), but the parsable files still get job rows, so the failure is visible
// in the UI.
func (d *Dispatcher) recordRun(ctx context.Context, ev Event, dir string, files []repo.TreeEntry) {
	run, err := d.runs.CreateRun(ctx, ev.RepoID, ev.Ref, ev.NewSHA)
	if err != nil {
		d.logger.Error("ci: create run failed",
			"repo", ev.RepoName, "ref", ev.Ref, "sha", ev.NewSHA, "error", err)
		return
	}
	var parseErr error
	for _, f := range files {
		if err := d.validate(ctx, dir, ev.NewSHA, f.Path); err != nil {
			if parseErr == nil {
				parseErr = err
			}
			continue // a malformed file gets no job row
		}
		if _, err := d.runs.CreateJob(ctx, run.ID, f.Name); err != nil {
			d.logger.Error("ci: create job failed", "run", run.ID, "job", f.Name, "error", err)
		}
	}
	if parseErr != nil {
		d.logger.Error("ci: malformed workflow",
			"repo", ev.RepoName, "ref", ev.Ref, "error", parseErr)
		if err := d.runs.SetRunStatus(ctx, run.ID, meta.RunFailed); err != nil {
			d.logger.Error("ci: set run failed", "run", run.ID, "error", err)
		}
	}
}

// recordErrorRun records a run in the error state for a discovery failure, so a
// broken push-CI surfaces instead of vanishing.
func (d *Dispatcher) recordErrorRun(ctx context.Context, ev Event) {
	run, err := d.runs.CreateRun(ctx, ev.RepoID, ev.Ref, ev.NewSHA)
	if err != nil {
		d.logger.Error("ci: create error run failed", "repo", ev.RepoName, "error", err)
		return
	}
	if err := d.runs.SetRunStatus(ctx, run.ID, meta.RunError); err != nil {
		d.logger.Error("ci: set run error failed", "run", run.ID, "error", err)
	}
}

// validate cheaply gates a workflow file: it must read and parse as YAML. Deep
// validation (does the engine accept it) is the runner's job at execution time.
func (d *Dispatcher) validate(ctx context.Context, dir, sha, path string) error {
	content, _, _, err := repo.Blob(ctx, dir, sha, path)
	if err != nil {
		return err
	}
	var doc map[string]any
	return yaml.Unmarshal(content, &doc)
}

// isAbsent reports whether a wire SHA denotes "no object" — the empty string or
// git's all-zero id (a ref deletion).
func isAbsent(sha string) bool {
	return sha == "" || sha == meta.ZeroSHA
}

// isYAML reports whether name has a workflow file extension.
func isYAML(name string) bool {
	return strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml")
}
