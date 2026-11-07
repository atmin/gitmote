// Package ci is the continuous-integration dispatch seam. On a successful push,
// the write path hands each updated ref to Dispatcher.Dispatch. Dispatch reads
// the .gitmote/workflows/ directory of the pushed commit. When there is work, it
// records a run with one job per workflow file.
//
// Dispatch is fire-and-forget: it must never block or fail a push. A missed
// run is a missed deploy, not a failed push. This is the content-before-pointer
// discipline applied to CI (see docs/architecture/safety.md and
// docs/architecture/ci.md).
package ci

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/atmin/gitmote/internal/meta"
	"github.com/atmin/gitmote/internal/repo"
	"gopkg.in/yaml.v3"
)

const (
	// branchPrefix is the ref namespace that CI reacts to. CI ignores tags and
	// other refs.
	branchPrefix = "refs/heads/"
	// workflowDir is the fixed, traversal-safe location that the engine (act)
	// reads. act reads the directory through `--workflows` (see
	// internal/runner). The YAML is unchanged.
	//
	// gitmote deliberately reads .gitmote/workflows, NOT .github/workflows.
	// GitHub only ever sees .github. So the directory of a workflow *declares*
	// which forge runs it. A repo mirrored to GitHub puts CI in .gitmote to run
	// only here, or in .github to run only there. Either way, no workflow runs
	// twice.
	//
	// The gitmote repo itself keeps its CI and GitHub deploy in .github. Only
	// the self-deploy workflow lives in .gitmote, so only that one runs on
	// gitmote.
	workflowDir = ".gitmote/workflows"
	// cloneTokenTTL bounds the per-run clone credential to one hour. It is the
	// maximum duration of a run, plus a margin. The token is read-only and
	// repo-scoped. So it grants no more than a fetch of the one repo. It stays
	// valid, and reusable, until it expires.
	cloneTokenTTL = time.Hour
)

// Runs is the part of meta that the dispatcher writes through. *meta.Metadata
// satisfies it.
type Runs interface {
	CreateRun(ctx context.Context, repoID int64, ref, sha string) (*meta.Run, error)
	SetRunStatus(ctx context.Context, runID int64, status meta.RunStatus) error
	CreateJob(ctx context.Context, runID int64, name string) (*meta.Job, error)
	SetJobStatus(ctx context.Context, jobID int64, status meta.RunStatus) error
}

// Materializer turns a repo name into a bare repo on disk that the browse
// reader can query. *repo.Materializer satisfies it.
type Materializer interface {
	Materialize(ctx context.Context, name string) (string, error)
}

// Trigger starts the runner of one CI job with the given env. Two types satisfy
// it: *scaleway.JobsClient (cloud) and *LocalTrigger (local dev). The Scaleway
// client does nothing when it is not configured.
type Trigger interface {
	Trigger(ctx context.Context, env map[string]string) error
}

// Minter mints the per-run clone credential. The credential is a read-only,
// repo-scoped, expiring token. The runner uses it to fetch the repo over the
// normal git-HTTP path. *auth.Guard satisfies it. A nil Minter disables
// minting, and the clone token stays empty.
type Minter interface {
	MintScoped(ctx context.Context, userID int64, label string, repoScope *int64, readOnly bool, expiresAt time.Time) (string, error)
}

// Secrets decrypts the CI secrets of a repo into a map from name to value. The
// dispatcher injects the map at trigger time. *secrets.Service satisfies it. A
// nil Secrets injects no secrets.
type Secrets interface {
	Secrets(ctx context.Context, repoID int64) (map[string]string, error)
}

// Event is one updated ref from a completed push.
type Event struct {
	RepoID   int64
	RepoName string
	Ref      string
	OldSHA   string
	NewSHA   string
	// PusherID is the authenticated user that pushed. The dispatcher mints the
	// clone token under this user, scoped read-only to this repo. The pusher
	// necessarily holds read access to the repo. So CI needs no separate
	// service identity and no ACL grant.
	PusherID int64
}

// Config wires the collaborators of the dispatcher. The dispatcher injects
// BaseURL and WorkerSecret into the env of each triggered runner. The runner
// clones from BaseURL. It authenticates its report-back with WorkerSecret.
type Config struct {
	Runs         Runs
	Materializer Materializer
	Trigger      Trigger
	Minter       Minter  // mints the per-run clone token. Nil leaves it empty.
	Secrets      Secrets // decrypts per-repo CI secrets. Nil injects none.
	// EnvSecrets are per-repo CI secrets that the operator supplies in the
	// process env (GITMOTE_REPO_SECRET_<REPO>__<NAME>). They overlay the DB
	// secrets and win over them. Empty is the common case: no env seeding.
	EnvSecrets   EnvSecrets
	BaseURL      string // GITMOTE_URL
	WorkerSecret string // WORKER_SECRET
	Logger       *slog.Logger
}

// Dispatcher enqueues CI runs for ref advances and triggers a runner per job.
// Every instance constructs one. Only the leader dispatches pushes, because the
// leader is the only instance that processes receive-pack.
type Dispatcher struct {
	runs       Runs
	mz         Materializer
	trigger    Trigger
	minter     Minter
	secrets    Secrets
	envSecrets EnvSecrets
	baseURL    string
	secret     string
	logger     *slog.Logger
	now        func() time.Time // injectable clock for the clone-token expiry
}

// NewDispatcher returns a Dispatcher from cfg.
func NewDispatcher(cfg Config) *Dispatcher {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Dispatcher{
		runs:       cfg.Runs,
		mz:         cfg.Materializer,
		trigger:    cfg.Trigger,
		minter:     cfg.Minter,
		secrets:    cfg.Secrets,
		envSecrets: cfg.EnvSecrets,
		baseURL:    cfg.BaseURL,
		secret:     cfg.WorkerSecret,
		logger:     logger,
		now:        time.Now,
	}
}

// Dispatch reads the workflows of the pushed commit. It records a run for a
// branch create or update that has any workflows. Tag pushes and branch
// deletions do nothing. A repo with no .gitmote/workflows creates no run.
// Everything here is non-fatal. The push has already committed, so Dispatch
// logs errors and never returns them.
func (d *Dispatcher) Dispatch(ctx context.Context, ev Event) {
	if !strings.HasPrefix(ev.Ref, branchPrefix) {
		return // not a branch, so no CI
	}
	if isAbsent(ev.NewSHA) {
		return // a branch deletion, so nothing to run
	}

	dir, err := d.mz.Materialize(ctx, ev.RepoName)
	if err != nil {
		// This is an unexpected materialize error. Record a visible errored run
		// rather than silently dropping the CI of the push.
		d.logger.Error("ci: materialize failed",
			"repo", ev.RepoName, "ref", ev.Ref, "sha", ev.NewSHA, "error", err)
		d.recordErrorRun(ctx, ev)
		return
	}

	entries, err := repo.Tree(ctx, dir, ev.NewSHA, workflowDir)
	if errors.Is(err, repo.ErrNotFound) {
		d.logger.Debug("ci: no workflows", "repo", ev.RepoName, "ref", ev.Ref)
		return // no .gitmote/workflows, so no run
	}
	if err != nil {
		d.logger.Error("ci: read workflows failed",
			"repo", ev.RepoName, "ref", ev.Ref, "sha", ev.NewSHA, "error", err)
		d.recordErrorRun(ctx, ev)
		return
	}

	branch := strings.TrimPrefix(ev.Ref, branchPrefix)
	var files []repo.TreeEntry
	for _, e := range entries {
		if e.Type == "blob" && isYAML(e.Name) && d.triggeredBy(ctx, dir, ev.NewSHA, e.Path, branch) {
			files = append(files, e)
		}
	}
	if len(files) == 0 {
		// No workflow is configured to run for this branch. Either none is
		// present, or all their `on.push` branch filters exclude it. Like GitHub,
		// record no run. A run appears only when something is meant to execute.
		return
	}

	d.recordRun(ctx, ev, dir, files)
}

// triggeredBy reports whether the workflow at path should run for a push to
// branch, according to its `on.push` branch filter. It keeps a file that can't
// be read or parsed (it returns true). So recordRun still surfaces that file as
// a failed run rather than silently dropping it. triggeredBy filters out only a
// well-formed workflow that does not select this branch or does not react to
// push.
func (d *Dispatcher) triggeredBy(ctx context.Context, dir, sha, path, branch string) bool {
	content, _, _, err := repo.Blob(ctx, dir, sha, path)
	if err != nil {
		return true
	}
	trig, err := parsePushTrigger(content)
	if err != nil {
		return true
	}
	return trig.matches(branch)
}

// recordRun creates a queued run and one job per workflow file. It then
// triggers a runner per job. Nothing here fails the push.
//
// A file that fails to parse as YAML downgrades the run to failed, and
// recordRun logs the first parse error. The parsable files still get job rows,
// so the failure is visible in the UI.
//
// A trigger error marks that job error. If every job failed to trigger, it
// also marks the run error. The failure of one job never aborts the others.
func (d *Dispatcher) recordRun(ctx context.Context, ev Event, dir string, files []repo.TreeEntry) {
	run, err := d.runs.CreateRun(ctx, ev.RepoID, ev.Ref, ev.NewSHA)
	if err != nil {
		d.logger.Error("ci: create run failed",
			"repo", ev.RepoName, "ref", ev.Ref, "sha", ev.NewSHA, "error", err)
		return
	}
	// Mint one read-only, repo-scoped, expiring clone token for the whole run.
	// Every job of a run clones the same repo at the same SHA. A mint failure is
	// non-fatal. The run still records, and the clone of the runner fails
	// visibly.
	cloneToken := d.mintCloneToken(ctx, ev, run.ID)
	// Decrypt the CI secrets of the repo once for the run, and inject them into
	// the env of every job. An error is non-fatal (fire-and-forget). The run
	// proceeds without the secrets.
	secretsEnv := d.repoSecrets(ctx, ev)
	var (
		parseErr      error
		jobs          int
		triggerErrors int
	)
	for _, f := range files {
		if err := d.validate(ctx, dir, ev.NewSHA, f.Path); err != nil {
			if parseErr == nil {
				parseErr = err
			}
			continue // a malformed file gets no job row
		}
		job, err := d.runs.CreateJob(ctx, run.ID, f.Name)
		if err != nil {
			d.logger.Error("ci: create job failed", "run", run.ID, "job", f.Name, "error", err)
			continue
		}
		jobs++
		if err := d.trigger.Trigger(ctx, d.jobEnv(ev, run.ID, job.ID, cloneToken, secretsEnv)); err != nil {
			d.logger.Error("ci: trigger failed",
				"run", run.ID, "job", job.ID, "name", f.Name, "error", err)
			if err := d.runs.SetJobStatus(ctx, job.ID, meta.RunError); err != nil {
				d.logger.Error("ci: set job error failed", "job", job.ID, "error", err)
			}
			triggerErrors++
		}
	}
	switch {
	case parseErr != nil:
		d.logger.Error("ci: malformed workflow",
			"repo", ev.RepoName, "ref", ev.Ref, "error", parseErr)
		d.setRunStatus(ctx, run.ID, meta.RunFailed)
	case jobs > 0 && triggerErrors == jobs:
		// Every job failed to trigger, so the run produced nothing.
		d.setRunStatus(ctx, run.ID, meta.RunError)
	}
}

// secretEnvPrefix namespaces an injected CI secret in the runner env. With it,
// the engine can tell a secret from the coordinates of the runner. The engine
// then exposes the secret to the workflow as `${{ secrets.NAME }}`. Keep it in
// sync with the ActEngine of the runner.
const secretEnvPrefix = "GITMOTE_CI_SECRET_"

// jobEnv is the per-job environment that the dispatcher injects into the
// triggered runner. It holds:
//   - the CI secrets of the repo, each namespaced under secretEnvPrefix so
//     the engine can forward them
//   - the coordinates that the runner claims and reports against
//   - the scoped clone token
func (d *Dispatcher) jobEnv(ev Event, runID, jobID int64, cloneToken string, secretsEnv map[string]string) map[string]string {
	env := make(map[string]string, len(secretsEnv)+8)
	for k, v := range secretsEnv {
		env[secretEnvPrefix+k] = v
	}
	env["GITMOTE_CI_RUN_ID"] = strconv.FormatInt(runID, 10)
	env["GITMOTE_CI_JOB_ID"] = strconv.FormatInt(jobID, 10)
	env["GITMOTE_URL"] = d.baseURL
	env["WORKER_SECRET"] = d.secret
	env["GITMOTE_REPO"] = ev.RepoName
	env["GITMOTE_SHA"] = ev.NewSHA
	env["GITMOTE_REF"] = ev.Ref
	env["GITMOTE_CI_CLONE_TOKEN"] = cloneToken
	return env
}

// repoSecrets assembles the CI secrets of the repo for injection. It starts
// with the encrypted secrets stored in the DB, when a service is configured.
// It overlays them with any env secrets that the operator supplies
// (GITMOTE_REPO_SECRET_*). The env secrets win on a name collision and need no
// master key. It returns nil when there are no secrets.
//
// repoSecrets never fails the fire-and-forget dispatch. A decrypt error drops
// the DB secrets and logs the error, but env secrets still apply. The workflow
// surfaces any value it is missing.
func (d *Dispatcher) repoSecrets(ctx context.Context, ev Event) map[string]string {
	var m map[string]string
	if d.secrets != nil {
		db, err := d.secrets.Secrets(ctx, ev.RepoID)
		if err != nil {
			d.logger.Error("ci: load secrets failed; running without them",
				"repo", ev.RepoName, "error", err)
		} else {
			m = db
		}
	}
	env := d.envSecrets.For(ev.RepoName)
	if len(env) == 0 {
		return m
	}
	if m == nil {
		m = make(map[string]string, len(env))
	}
	for name, val := range env {
		m[name] = val // env overrides DB on collision
	}
	return m
}

// mintCloneToken mints the per-run clone token under the identity of the
// pusher. The token is read-only, repo-scoped and expiring. It returns "" in
// two cases: no minter is configured, or the mint fails (and it logs). A
// missing token is non-fatal here. The clone of the runner fails, and the job
// is reported errored. It never causes a failed push.
func (d *Dispatcher) mintCloneToken(ctx context.Context, ev Event, runID int64) string {
	if d.minter == nil {
		return ""
	}
	repoID := ev.RepoID
	label := "ci-clone run " + strconv.FormatInt(runID, 10)
	tok, err := d.minter.MintScoped(ctx, ev.PusherID, label, &repoID, true, d.now().Add(cloneTokenTTL))
	if err != nil {
		d.logger.Error("ci: mint clone token failed",
			"repo", ev.RepoName, "run", runID, "error", err)
		return ""
	}
	return tok
}

// setRunStatus transitions a run, logging (not returning) any error.
func (d *Dispatcher) setRunStatus(ctx context.Context, runID int64, status meta.RunStatus) {
	if err := d.runs.SetRunStatus(ctx, runID, status); err != nil {
		d.logger.Error("ci: set run status failed", "run", runID, "status", status, "error", err)
	}
}

// recordErrorRun records a run in the error state for a discovery failure. So
// broken CI for a push surfaces instead of vanishing.
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

// validate cheaply gates a workflow file: the file must read and parse as YAML.
// Deep validation checks whether the engine accepts the file. The runner does
// that at execution time.
func (d *Dispatcher) validate(ctx context.Context, dir, sha, path string) error {
	content, _, _, err := repo.Blob(ctx, dir, sha, path)
	if err != nil {
		return err
	}
	var doc map[string]any
	return yaml.Unmarshal(content, &doc)
}

// isAbsent reports whether a wire SHA denotes "no object". That is the empty
// string or the all-zero id of git (a ref deletion).
func isAbsent(sha string) bool {
	return sha == "" || sha == meta.ZeroSHA
}

// isYAML reports whether name has a workflow file extension.
func isYAML(name string) bool {
	return strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml")
}
