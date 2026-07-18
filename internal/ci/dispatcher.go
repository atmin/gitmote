// Package ci is the continuous-integration dispatch seam. On a successful push
// the write path hands each updated ref to Dispatcher.Dispatch, which reads the
// pushed commit's .gitmote/workflows/ directory and — when there is work —
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
	"strconv"
	"strings"
	"time"

	"github.com/atmin/gitmote/internal/meta"
	"github.com/atmin/gitmote/internal/repo"
	"gopkg.in/yaml.v3"
)

const (
	// branchPrefix is the ref namespace CI reacts to; tags and other refs are
	// ignored.
	branchPrefix = "refs/heads/"
	// workflowDir is the fixed, traversal-safe location the engine (act) reads.
	// gitmote deliberately reads .gitmote/workflows, NOT .github/workflows: GitHub
	// only ever sees .github, so which directory a workflow lives in *declares*
	// which forge runs it — a repo mirrored to GitHub puts CI in .gitmote to run
	// only here, in .github to run only there, and gets no double-run. (This is
	// also why gitmote's own repo, which has only .github, never CI's on gitmote.)
	// act reads it via `--workflows` (see internal/runner); the YAML is unchanged.
	workflowDir = ".gitmote/workflows"
	// cloneTokenTTL bounds the per-run clone credential: a run's max duration
	// plus margin. The token is read-only and repo-scoped, so it grants no more
	// than a fetch of the one repo, and expires long before it could be reused.
	cloneTokenTTL = time.Hour
)

// Runs is the slice of meta the dispatcher writes through. *meta.Metadata
// satisfies it.
type Runs interface {
	CreateRun(ctx context.Context, repoID int64, ref, sha string) (*meta.Run, error)
	SetRunStatus(ctx context.Context, runID int64, status meta.RunStatus) error
	CreateJob(ctx context.Context, runID int64, name string) (*meta.Job, error)
	SetJobStatus(ctx context.Context, jobID int64, status meta.RunStatus) error
}

// Materializer turns a repo name into an on-disk bare repo the browse reader can
// query. *repo.Materializer satisfies it.
type Materializer interface {
	Materialize(ctx context.Context, name string) (string, error)
}

// Trigger starts one CI job's runner with the given env. *scaleway.JobsClient
// (cloud) and *LocalTrigger (local dev) both satisfy it; the Scaleway client
// no-ops when unconfigured.
type Trigger interface {
	Trigger(ctx context.Context, env map[string]string) error
}

// Minter mints the per-run clone credential: a read-only, repo-scoped, expiring
// token the runner uses to fetch the repo over the normal git-HTTP path.
// *auth.Guard satisfies it. Nil disables minting (the clone token is left empty).
type Minter interface {
	MintScoped(ctx context.Context, userID int64, label string, repoScope *int64, readOnly bool, expiresAt time.Time) (string, error)
}

// Secrets decrypts a repo's CI secrets into a name→value map to inject at
// trigger. *secrets.Service satisfies it. Nil injects no secrets.
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
	// PusherID is the authenticated user that pushed. The clone token is minted
	// under it (scoped read-only to this repo), since the pusher necessarily holds
	// repo read — no separate CI service identity or ACL grant is needed.
	PusherID int64
}

// Config wires the dispatcher's collaborators. BaseURL and WorkerSecret are
// injected into each triggered runner's env (the runner clones from BaseURL and
// authenticates its report-back with WorkerSecret — stage 4).
type Config struct {
	Runs         Runs
	Materializer Materializer
	Trigger      Trigger
	Minter       Minter  // mints the per-run clone token; nil leaves it empty
	Secrets      Secrets // decrypts per-repo CI secrets to inject; nil injects none
	// EnvSecrets are operator-supplied per-repo CI secrets read from the process
	// env (GITMOTE_REPO_SECRET_<REPO>__<NAME>); overlaid on and winning over the
	// DB secrets. Empty is the common case (no env seeding).
	EnvSecrets   EnvSecrets
	BaseURL      string // GITMOTE_URL
	WorkerSecret string // WORKER_SECRET
	Logger       *slog.Logger
}

// Dispatcher enqueues CI runs for ref advances and triggers a runner per job.
// Only the leader constructs and calls it (it is the only instance that
// processes receive-pack).
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

// Dispatch reads the pushed commit's workflows and records a run for a branch
// create/update that has any. Tag pushes and branch deletions do nothing; a repo
// with no .gitmote/workflows creates no run. Everything here is non-fatal: the
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
		return // no .gitmote/workflows → no run
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
		// No workflow is configured to run for this branch (none present, or their
		// `on.push` branch filters all exclude it). Like GitHub, record no run —
		// a run appears only when something is actually meant to execute.
		return
	}

	d.recordRun(ctx, ev, dir, files)
}

// triggeredBy reports whether the workflow at path should run for a push to
// branch, per its `on.push` branch filter. A file that can't be read or parsed
// is kept (returns true) so recordRun still surfaces it as a failed run rather
// than silently dropping it — only a well-formed workflow that genuinely does
// not select this branch (or doesn't react to push) is filtered out.
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

// recordRun creates a queued run and one job per workflow file, then triggers a
// runner per job. A file that fails to parse as YAML downgrades the run to failed
// (the first parse error is logged), but the parsable files still get job rows,
// so the failure is visible in the UI. A trigger error marks that job error and,
// if every job failed to trigger, the run error — one job's failure never aborts
// the others, and nothing here fails the push.
func (d *Dispatcher) recordRun(ctx context.Context, ev Event, dir string, files []repo.TreeEntry) {
	run, err := d.runs.CreateRun(ctx, ev.RepoID, ev.Ref, ev.NewSHA)
	if err != nil {
		d.logger.Error("ci: create run failed",
			"repo", ev.RepoName, "ref", ev.Ref, "sha", ev.NewSHA, "error", err)
		return
	}
	// Mint one read-only, repo-scoped, expiring clone token for the whole run;
	// every job of a run clones the same repo at the same SHA. A mint failure is
	// non-fatal — the run still records, and the runner's clone will fail visibly.
	cloneToken := d.mintCloneToken(ctx, ev, run.ID)
	// Decrypt the repo's CI secrets once for the run; injected into every job's
	// env. Non-fatal on error (fire-and-forget) — the run proceeds without them.
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
		// Every job failed to trigger — the run produced nothing.
		d.setRunStatus(ctx, run.ID, meta.RunError)
	}
}

// secretEnvPrefix namespaces an injected CI secret in the runner env, so the
// engine can tell a secret from the runner's own coordinates and expose it to the
// workflow (as `${{ secrets.NAME }}`). Kept in sync with the runner's ActEngine.
const secretEnvPrefix = "GITMOTE_CI_SECRET_"

// jobEnv is the per-job environment injected into the triggered runner: the
// repo's CI secrets (each namespaced under secretEnvPrefix so the engine can
// forward them), plus the coordinates it claims and reports against and the
// scoped clone token.
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

// repoSecrets assembles the repo's CI secrets for injection: the encrypted,
// DB-stored secrets (when a service is configured) overlaid with any
// operator-supplied env secrets (GITMOTE_REPO_SECRET_*), which win on a name
// collision and need no master key. It never fails the fire-and-forget dispatch —
// a decrypt error drops the DB secrets (logged) but env secrets still apply, and
// the workflow surfaces any value it is missing. Returns nil when there are none.
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

// mintCloneToken mints the per-run read-only, repo-scoped, expiring clone token
// under the pusher's identity. It returns "" (and logs) when no minter is
// configured or the mint fails — a missing token is non-fatal here; the runner's
// clone simply fails and the job is reported errored, never a failed push.
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
