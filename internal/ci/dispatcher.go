// Package ci is the continuous-integration dispatch seam. On a successful push
// the write path hands each updated ref to Dispatcher.Dispatch, which — for a
// branch create/update — records a queued run in meta. It is fire-and-forget:
// dispatch must never block or fail a push (a missed run is a missed deploy, not
// a failed push — the content-before-pointer discipline applied to CI; see
// docs/architecture/safety.md and tasks/16-ci.md). Later stages extend Dispatch
// with config discovery and the Scaleway trigger.
package ci

import (
	"context"
	"log/slog"
	"strings"

	"github.com/atmin/gitmote/internal/meta"
)

// branchPrefix is the ref namespace CI reacts to; tags and other refs are
// ignored.
const branchPrefix = "refs/heads/"

// Runs is the slice of meta the dispatcher needs to enqueue runs. *meta.Metadata
// satisfies it; a fake stands in for unit tests.
type Runs interface {
	CreateRun(ctx context.Context, repoID int64, ref, sha string) (*meta.Run, error)
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
	logger *slog.Logger
}

// NewDispatcher returns a Dispatcher writing runs through runs.
func NewDispatcher(runs Runs, logger *slog.Logger) *Dispatcher {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Dispatcher{runs: runs, logger: logger}
}

// Dispatch records a queued run for a branch create/update. Tag pushes and
// branch deletions create no run. An error creating the run is logged, not
// returned — the push has already committed.
func (d *Dispatcher) Dispatch(ctx context.Context, ev Event) {
	if !strings.HasPrefix(ev.Ref, branchPrefix) {
		return // not a branch — no CI
	}
	if isAbsent(ev.NewSHA) {
		return // a branch deletion — nothing to run
	}
	if _, err := d.runs.CreateRun(ctx, ev.RepoID, ev.Ref, ev.NewSHA); err != nil {
		d.logger.Error("ci: enqueue run failed",
			"repo", ev.RepoName, "ref", ev.Ref, "sha", ev.NewSHA, "error", err)
	}
}

// isAbsent reports whether a wire SHA denotes "no object" — the empty string or
// git's all-zero id (a ref deletion).
func isAbsent(sha string) bool {
	return sha == "" || sha == meta.ZeroSHA
}
