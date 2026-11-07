// Command gitmote is the gitmote server binary.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/atmin/gitmote/internal/auth"
	"github.com/atmin/gitmote/internal/bootstrap"
	"github.com/atmin/gitmote/internal/ci"
	"github.com/atmin/gitmote/internal/githttp"
	"github.com/atmin/gitmote/internal/meta"
	"github.com/atmin/gitmote/internal/repo"
	"github.com/atmin/gitmote/internal/scaleway"
	"github.com/atmin/gitmote/internal/secrets"
	"github.com/atmin/gitmote/internal/store"
	"github.com/atmin/gitmote/internal/webui"
	"github.com/atmin/s3lite"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const shutdownTimeout = 10 * time.Second

// promoteTimeout bounds an on-demand lease acquisition on the request path. See
// the gate in buildGitHandler. A follower waits at most this long for the
// restore, and then it falls back to a 503. A slow S3 can't hang a request.
const promoteTimeout = 10 * time.Second

const (
	// idleYieldAfter is how long a writer serves no request before it hands the
	// lease back. Another instance on the same storage root can then write next.
	// See leaseActivity and docs/configuration.md.
	idleYieldAfter = 5 * time.Second
	// yieldCheckInterval is how often the writer checks whether it is idle.
	yieldCheckInterval = time.Second
	// yieldTimeout bounds one YieldLease call, which is the final sync and the
	// release.
	yieldTimeout = 10 * time.Second
)

// defaultListenAddr is the bind address in the container, where ports are mapped
// externally. GITMOTE_LISTEN_ADDR overrides it. Use the override to run a second
// native instance beside `make dev`, for example the break-glass self-deploy on
// :8081.
const defaultListenAddr = ":8080"

// listenAddrFromEnv returns GITMOTE_LISTEN_ADDR when it is set, and
// defaultListenAddr otherwise.
func listenAddrFromEnv() string {
	if v := os.Getenv("GITMOTE_LISTEN_ADDR"); v != "" {
		return v
	}
	return defaultListenAddr
}

const (
	// reconcileInterval is how often the leader sweeps abandoned CI jobs.
	reconcileInterval = 5 * time.Minute
	// reconcileMaxAge bounds a running job. Past this age, the runner is presumed
	// dead and its job is failed.
	reconcileMaxAge = time.Hour
)

func main() {
	// A subcommand runs without the server, as a single-writer admin task. With
	// no subcommand, the binary runs the server.
	if args := os.Args[1:]; len(args) > 0 && args[0] == "bootstrap" {
		if err := runBootstrap(context.Background(), args[1:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "bootstrap:", err)
			os.Exit(1)
		}
		return
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	if err := run(context.Background(), logger, listenAddrFromEnv()); err != nil {
		logger.Error("server failed", "error", err)
		os.Exit(1)
	}
}

// runBootstrap creates the first admin and token on an empty instance. With
// -reissue, it mints a fresh token for the existing admin instead. It opens the
// metadata DB as the environment configures it: the data dir, and the replica
// derived from the bucket. On success, it prints the one-time token to out. The
// deferred Close durably flushes replication. This short-lived process thus
// reliably pushes the new admin and token to S3, and the server restores them.
func runBootstrap(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	handle := fs.String("handle", bootstrap.DefaultAdminHandle, "admin user handle")
	repoName := fs.String("repo", "", "optional initial repository, e.g. gitmote (repos are otherwise made in the UI)")
	branch := fs.String("default-branch", "main", "default branch for the initial repo")
	reissue := fs.Bool("reissue", false, "mint a fresh token for the existing admin (recover a lost token)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Bootstrap is a deliberate single-shot writer. When a replica is configured,
	// RoleWriter acquires the lease. It fails loudly if a live server already
	// holds the lease, so bootstrapping can never race the running writer.
	md, err := meta.Open(ctx, metaConfigFromEnv(nil, s3lite.RoleWriter))
	if err != nil {
		return fmt.Errorf("open metadata: %w", err)
	}
	defer func() { _ = md.Close() }()

	opts := bootstrap.Options{AdminHandle: *handle, RepoName: *repoName, DefaultBranch: *branch}
	if *reissue {
		res, err := bootstrap.Reissue(ctx, md, opts)
		if err != nil {
			return err
		}
		return writeBootstrapBanner(out, res)
	}

	res, err := bootstrap.Run(ctx, md, opts)
	if err != nil {
		return err
	}
	if res.AlreadyBootstrapped {
		_, err := io.WriteString(out, "already bootstrapped: an admin exists; nothing to do "+
			"(run with -reissue to mint a fresh token)\n")
		return err
	}
	return writeBootstrapBanner(out, res)
}

// writeBootstrapBanner prints the one-time admin token inside a large,
// self-explanatory banner. The banner says:
//   - what the token is
//   - how to sign in and clone
//   - that the token won't be shown again
//   - how to mint a new token if the token is lost
//
// The CLI and the first-run auto-bootstrap use it. Its output is the operator's
// only chance to capture the token.
func writeBootstrapBanner(out io.Writer, res *bootstrap.Result) error {
	const bar = "========================================================================"
	repoLines := fmt.Sprintf(
		"  Repos:    create one in the UI, then clone:\n"+
			"            git clone http://%s:<token>@<host>/<repo>\n",
		res.Admin.Handle)
	if res.Repo != nil {
		repoLines = fmt.Sprintf(
			"  Clone:    git clone http://%s:<token>@<host>/%s\n",
			res.Admin.Handle, res.Repo.Name)
	}
	_, err := fmt.Fprintf(out, "\n%s\n"+
		"  gitmote access token — SAVE IT NOW (shown only once)\n"+
		"%s\n\n"+
		"  admin user:   %s\n\n"+
		"  access token:\n\n      %s\n\n"+
		"  Sign in:  open  http://<host>/login  and paste the token\n"+
		"%s"+
		"\n  This token is NOT stored and will NOT be shown again.\n"+
		"  Lost it? Stop the server (scale to zero) and run:  gitmote bootstrap -reissue\n"+
		"  Then rotate: sign in, mint your own token, and revoke this one.\n"+
		"%s\n\n",
		bar, bar, res.Admin.Handle, res.RawToken, repoLines, bar)
	return err
}

// maybeAutoBootstrap makes a fresh instance usable without a second command. On
// the leader with no admin yet, it creates the admin and mints a token. It logs
// the token once, inside a large banner. That output is the operator's only
// chance to capture the token. On a later boot (an admin exists) and on a
// follower, it does nothing. A first-ever boot is uncontended, so that instance
// is always the leader.
//
// It never creates a repo, because repos are made in the UI. A failure is not
// fatal: it is logged, not returned, so the server still serves. The operator
// can then bootstrap by hand. If an admin was created without a token, the
// operator recovers with `bootstrap -reissue`.
func maybeAutoBootstrap(ctx context.Context, md *meta.Metadata, logger *slog.Logger) {
	if !md.IsLeader() {
		return
	}
	exists, err := md.AdminExists(ctx)
	if err != nil {
		logger.Error("auto-bootstrap: check for an admin failed", "error", err)
		return
	}
	if exists {
		return
	}
	res, err := bootstrap.Run(ctx, md, bootstrap.Options{AdminHandle: bootstrap.DefaultAdminHandle})
	if err != nil {
		logger.Error("auto-bootstrap failed; bootstrap by hand", "error", err)
		return
	}
	if res.AlreadyBootstrapped {
		return // another writer won the race and bootstrapped first
	}
	logger.Info("auto-bootstrapped the first admin; the one-time token is printed below", "admin", res.Admin.Handle)
	if err := writeBootstrapBanner(os.Stderr, res); err != nil {
		logger.Error("auto-bootstrap: printing the token banner failed", "error", err)
	}
}

// run starts the HTTP server. It blocks until ctx is cancelled or a SIGINT or
// SIGTERM arrives, and then it shuts the server down gracefully.
func run(ctx context.Context, logger *slog.Logger, addr string) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	act := newLeaseActivity()
	gitHandler, ui, reportAPI, gate, closeMeta, err := buildGitHandler(ctx, logger, act)
	if err != nil {
		return err
	}
	defer func() {
		if err := closeMeta(); err != nil {
			logger.Error("closing metadata", "error", err)
		}
	}()

	srv := &http.Server{
		Addr:    addr,
		Handler: act.track(newHandler(gitHandler, ui, reportAPI, gate)),
	}

	if reportAPI != nil {
		go reconcileLoop(ctx, reportAPI, act, gate, logger)
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", addr, "version", version)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// reconcileLoop periodically sweeps abandoned CI jobs. A runner that dies during
// a run leaves its job stuck in running. The loop stops when ctx is cancelled at
// shutdown.
func reconcileLoop(ctx context.Context, reportAPI *ci.ReportAPI, act *leaseActivity, gate func(context.Context) bool, logger *slog.Logger) {
	t := time.NewTicker(reconcileInterval)
	defer t.Stop()
	sweep := func(ctx context.Context) error {
		return reportAPI.ReconcileStuck(ctx, reconcileMaxAge, time.Now())
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := reconcileOnce(ctx, act, gate, sweep); err != nil {
				logger.Error("ci: reconcile sweep failed", "error", err)
			}
		}
	}
}

// reconcileOnce runs one sweep as the writer. An idle writer yields its lease
// (see leaseActivity). So the tick takes the lease through the gate, like a
// request does. It skips the sweep while another instance holds the lease. That
// instance sweeps on its own ticks. A nil gate (no replica) means that this
// instance is always the writer.
//
// reconcileOnce holds the activity read lock throughout, so the sweep never
// overlaps a yield. The sweep does not count as activity. So the lease is free
// again on the next idle check.
func reconcileOnce(ctx context.Context, act *leaseActivity, gate func(context.Context) bool, sweep func(context.Context) error) error {
	act.mu.RLock()
	defer act.mu.RUnlock()
	if gate != nil && !gate(ctx) {
		return nil
	}
	return sweep(ctx)
}

// leaseActivity lets an idle writer hand its lease back without fencing a
// request in flight. Each request that may touch the metadata holds mu for
// reading for its whole life. A yield takes mu for writing, so a request and a
// yield never overlap. A request that arrives during a yield waits, and then the
// gate takes the lease again. last is when the latest such request ended, in
// Unix nanoseconds.
type leaseActivity struct {
	mu   sync.RWMutex
	last atomic.Int64
}

// newLeaseActivity starts the idle clock now, so a fresh writer serves at least
// idleYieldAfter before it yields.
func newLeaseActivity() *leaseActivity {
	a := &leaseActivity{}
	a.last.Store(time.Now().UnixNano())
	return a
}

// track wraps next so that every request counts as activity. The exceptions are
// the liveness probes and static assets. They carry no metadata. Also, a health
// check every few seconds would otherwise keep the lease on this instance
// forever.
func (a *leaseActivity) track(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if alwaysServable(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		a.mu.RLock()
		defer func() {
			a.last.Store(time.Now().UnixNano())
			a.mu.RUnlock()
		}()
		next.ServeHTTP(w, r)
	})
}

// idle reports whether no request has ended within idleYieldAfter.
func (a *leaseActivity) idle() bool {
	return time.Since(time.Unix(0, a.last.Load())) >= idleYieldAfter
}

// maybeYield hands the lease back when all of these hold:
//   - this instance is the writer
//   - no request is in flight
//   - no request has ended within idleYieldAfter
//
// It reports whether it yielded. After a failed yield, s3lite keeps or drops the
// lease. It either renews, or it demotes and fires OnDemote. In both cases, the
// next check tries again.
func (a *leaseActivity) maybeYield(ctx context.Context, isLeader func() bool, yield func(context.Context) error, logger *slog.Logger) bool {
	if !isLeader() || !a.idle() {
		return false
	}
	if !a.mu.TryLock() {
		return false // a request is in flight
	}
	defer a.mu.Unlock()
	if !a.idle() { // a request ended between the check and the lock
		return false
	}
	yctx, cancel := context.WithTimeout(ctx, yieldTimeout)
	defer cancel()
	if err := yield(yctx); err != nil {
		if !errors.Is(err, s3lite.ErrNotLeader) {
			logger.Warn("yielding the idle writer lease failed", "error", err)
		}
		return false
	}
	logger.Info("yielded the writer lease after idle", "idle", idleYieldAfter)
	return true
}

// yieldWhenIdle runs maybeYield every yieldCheckInterval until ctx ends.
func yieldWhenIdle(ctx context.Context, act *leaseActivity, md *meta.Metadata, logger *slog.Logger) {
	t := time.NewTicker(yieldCheckInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			act.maybeYield(ctx, md.IsLeader, md.YieldLease, logger)
		}
	}
}

// newHandler returns the server's HTTP routes. When ui is non-nil, it registers
// the management UI routes, such as /login, /users and /{repo}/settings. These
// patterns are more specific, so they win over the git catch-all. When
// gitHandler is non-nil, it serves the git smart-HTTP endpoints at "/". The
// exact health and version routes stay more specific.
//
// gate decides whether this instance may answer a metadata-derived request (see
// leaderGate). A follower serves a frozen, stale snapshot. This happens after a
// deploy, or while another instance on the same storage root writes. So a
// follower must not answer with stale refs. The gate promotes on demand when it
// can. Otherwise, it refuses.
func newHandler(gitHandler http.Handler, ui *webui.Handler, reportAPI *ci.ReportAPI, gate func(context.Context) bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, version)
	})
	if ui != nil {
		ui.Register(mux)
	}
	if reportAPI != nil {
		reportAPI.Register(mux)
	}
	if gitHandler != nil {
		mux.Handle("/", gitHandler)
	}
	return leaderGate(mux, gate)
}

// leaderGate never serves stale data. Only the writer (leader) answers requests
// that read or write the metadata DB. A follower restored a snapshot at startup.
// It does not catch up until it promotes (s3lite RoleAuto), so its refs can lag
// the true tip. A browse would show a just-pushed file as missing, and a fetch
// would miss commits. So instead, a follower returns 503 with Retry-After.
//
// These paths must stay up on a follower:
//   - The liveness probes, which are Scaleway's health check. Gating them would
//     deadlock a rolling deploy. The new instance can't promote until the old
//     one drains.
//   - Static assets, which carry no metadata.
//
// A nil gate (unreplicated dev, sole writer) means that this instance is always
// the leader.
//
// The gate promotes on demand. During a graceful deploy, the old writer
// releases its lease. It does so before the first request reaches the new
// instance.
// An idle writer also yields its lease (see leaseActivity). In both cases, the
// gate acquires the lease here, blocks for the restore, and serves. It still
// returns 503 when another holder really has the lease:
//   - Another instance is busy, until it is idle for idleYieldAfter.
//   - An old instance was hard-killed, until its lease expires.
func leaderGate(next http.Handler, gate func(context.Context) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if alwaysServable(r.URL.Path) || gate == nil || gate(r.Context()) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Retry-After", "2")
		http.Error(w, "gitmote is not the writer right now (another instance holds the lease) — retry shortly",
			http.StatusServiceUnavailable)
	})
}

// alwaysServable reports whether a follower may answer a path. Those paths are
// the liveness probes and static assets, which carry no metadata.
func alwaysServable(p string) bool {
	return p == "/healthz" || p == "/version" || strings.HasPrefix(p, "/static/")
}

// buildGitHandler wires the git read and write paths when an object store is
// configured (GITMOTE_S3_BUCKET). Without an object store, the server runs only
// the health and version routes, as a dev convenience. The returned close func
// is then a no-op.
//
// The management UI (the second return value) is built whenever an object store
// is configured. The HMAC key that signs session cookies is GITMOTE_COOKIE_KEY,
// or an auto-generated key persisted in meta. The UI shares this metadata
// handle, so it runs beside the git server.
//
// The db, cache, and socket paths derive from GITMOTE_DATA (see dataDir). The
// metadata replicates to a replica derived from the storage root (see
// replicaTarget). The push path installs the pre-receive hook binary,
// gitmote-hook, which is beside this binary.
func buildGitHandler(ctx context.Context, logger *slog.Logger, act *leaseActivity) (http.Handler, *webui.Handler, *ci.ReportAPI, func(context.Context) bool, func() error, error) {
	noop := func() error { return nil }

	if os.Getenv("GITMOTE_S3_BUCKET") == "" {
		logger.Warn("GITMOTE_S3_BUCKET unset; git endpoints disabled (health/version only)")
		return nil, nil, nil, nil, noop, nil
	}

	// With RoleAuto, this instance becomes the writer when it can acquire the
	// lease. Otherwise, it is a read-only follower. During a rolling deploy, the
	// old and new instances overlap briefly. That overlap is one writer and one
	// follower, never two writers (docs/architecture/safety.md, section 1). With
	// no replica configured, the role has no effect, and the instance is the sole
	// writer. Tests and local dev therefore behave the same.
	md, err := meta.Open(ctx, metaConfigFromEnv(logger, s3lite.RoleAuto))
	if err != nil {
		return nil, nil, nil, nil, noop, fmt.Errorf("metadata: %w", err)
	}
	md.OnPromote(func() {
		logger.Info("became writer: acquired the lease", "generation", md.Generation())
	})
	md.OnDemote(func(err error) {
		logger.Warn("lost the writer lease; now read-only", "error", err)
	})

	// gate answers the question "may this instance serve a metadata request?".
	// It also promotes on demand when it can. The metadata opens with
	// OnDemandPromotion, so the gate is the only way that a follower becomes the
	// writer after Open.
	//
	// During a graceful deploy, the old writer has released its lease. An idle
	// writer also yields its lease (yieldWhenIdle). In both cases, TryPromote
	// acquires the lease here and blocks for the restore. OnPromote above logs
	// the transition. So two live instances on one storage root take turns as
	// the writer, and the lease keeps them from overlapping.
	//
	// The IsLeader fast path in TryPromote makes the steady-state call free.
	// TryPromote returns false, not an error, while another holder still has the
	// lease. So a follower under a busy writer stays quiet and answers 503. The
	// call is bounded, so a slow restore falls back to 503 and does not hang the
	// request.
	gate := func(reqCtx context.Context) bool {
		pctx, cancel := context.WithTimeout(reqCtx, promoteTimeout)
		defer cancel()
		ok, err := md.TryPromote(pctx)
		if err != nil {
			logger.Warn("on-demand promotion failed; refusing request", "error", err)
		}
		return ok
	}

	// Objects, CI logs, and the metadata replica all live under the storage root
	// (GITMOTE_S3_BUCKET, as "bucket" or "bucket/base"). So they share fate. If
	// the root is wrong, nothing restores, and the result is a clean, empty
	// instance. The result is never a half-restored state with refs but without
	// objects.
	objs, err := store.NewS3FromEnv(ctx)
	if err != nil {
		_ = md.Close()
		return nil, nil, nil, nil, noop, fmt.Errorf("object store: %w", err)
	}

	// The first-run auto-bootstrap makes a fresh instance on an empty bucket usable
	// without a second command. It runs before anything serves. It is not fatal:
	// a failure is logged, not returned.
	maybeAutoBootstrap(ctx, md, logger)

	writer, err := githttp.NewWriter(md, objs, hookBinaryPath(), sockPath(), logger)
	if err != nil {
		_ = md.Close()
		return nil, nil, nil, nil, noop, fmt.Errorf("write path: %w", err)
	}
	materializer := repo.New(md, objs, cachePath())
	guard := auth.NewGuard(md)

	// The CI secrets service reads its master keys from GITMOTE_CI_SECRET_KEY_V<n>.
	// A malformed key is fatal, so the server fails loudly. With no key set, the
	// service is disabled: there is no secrets UI, and no secret is injected. The
	// UI and the dispatcher both use this one service.
	keyring, err := secrets.NewKeyringFromEnv()
	if err != nil {
		_ = writer.Close()
		_ = md.Close()
		return nil, nil, nil, nil, noop, fmt.Errorf("ci secrets: %w", err)
	}
	secretsSvc := secrets.NewService(keyring, md)

	// Select the CI trigger. Cloud and local run the *same* runner code and env
	// contract. Only the substrate differs: Scaleway Serverless Jobs in
	// production, and a local process for dev (docs/architecture/ci.md).
	// Both need two settings:
	//   - a reachable GITMOTE_URL, which the runner uses to clone and report back
	//   - a WORKER_SECRET, which authenticates the report API
	//
	// With neither configured, runs are still recorded, but nothing executes.
	jobDefID := os.Getenv("SCW_CI_JOB_DEFINITION_ID")
	gitmoteURL := os.Getenv("GITMOTE_URL")
	// Cloud needs a reachable URL for the runner to clone and report back. If the
	// job definition is set without a URL, fail loudly before provisioning any
	// secret.
	if jobDefID != "" && gitmoteURL == "" {
		_ = writer.Close()
		_ = md.Close()
		return nil, nil, nil, nil, noop, fmt.Errorf("SCW_CI_JOB_DEFINITION_ID is set but GITMOTE_URL is missing")
	}
	// WORKER_SECRET authenticates the runner's report-back. It has no use without
	// a CI substrate, so resolve it lazily, only when a substrate is configured.
	// A plain run without CI provisions nothing.
	//
	// Take it from the env. Otherwise, get or create it in meta: it is generated
	// once and restored on every later boot. The secret reaches the cloud runner
	// by injection at trigger time. It is never baked into the job definition
	// (see jobEnv in the dispatcher).
	var workerSecret string
	if jobDefID != "" || gitmoteURL != "" {
		workerSecret, err = resolveWorkerSecret(ctx, md)
		if err != nil {
			_ = writer.Close()
			_ = md.Close()
			return nil, nil, nil, nil, noop, fmt.Errorf("worker secret: %w", err)
		}
	}
	var trigger ci.Trigger = ci.NoopTrigger{}
	switch {
	case jobDefID != "":
		// Without SCW_REGION, use the AWS_REGION already set for the object store.
		trigger = scaleway.NewJobsClient(os.Getenv("SCW_SECRET_KEY"), envOr("SCW_REGION", os.Getenv("AWS_REGION")), jobDefID)
		logger.Info("CI trigger: Scaleway Serverless Jobs", "job_definition_id", jobDefID)
	case gitmoteURL != "":
		trigger = ci.NewLocalTrigger(runnerBinaryPath(), logger)
		logger.Info("CI trigger: local runner", "runner", runnerBinaryPath(), "url", gitmoteURL)
		if allow, _ := strconv.ParseBool(os.Getenv("GITMOTE_CI_ALLOW_BUILDS")); allow {
			logger.Warn("GITMOTE_CI_ALLOW_BUILDS is on: CI can reach the host Docker daemon (host root) — trusted repos only")
		}
	default:
		logger.Warn("CI trigger disabled; runs record but do not execute " +
			"(set GITMOTE_URL for local, or SCW_CI_JOB_DEFINITION_ID + GITMOTE_URL for cloud)")
	}

	// A successful push that advances a branch does three things:
	//   - It discovers its workflows.
	//   - It records a run.
	//   - It triggers a runner for each job.
	//
	// Dispatch is fire-and-forget, so it never fails the push
	// (docs/architecture/ci.md).
	//
	// The operator can supply per-repo CI secrets in the process env, as
	// GITMOTE_REPO_SECRET_<REPO>__<NAME>. They are never stored, and they need no
	// master key. The automated self-deploy seeds them from a gitignored .env
	// (docs/deploy-scaleway.md).
	envSecrets := ci.LoadEnvSecrets(os.Environ())
	if red := envSecrets.Redacted(); len(red) > 0 {
		logger.Info("env CI secrets loaded (never persisted)", "repos", red)
	}
	dispatcher := ci.NewDispatcher(ci.Config{
		Runs:         md,
		Materializer: materializer,
		Trigger:      trigger,
		Minter:       guard,
		Secrets:      secretsSvc,
		EnvSecrets:   envSecrets,
		BaseURL:      gitmoteURL,
		WorkerSecret: workerSecret,
		Logger:       logger,
	})
	// The report API is the runner's authenticated API to claim and complete
	// jobs. Only the leader may write completions, and a follower returns a
	// retryable 503. The API authenticates with the same WORKER_SECRET that is
	// injected into the runner env.
	//
	// liveLogs is the in-memory live-log store. The runner streams into it, and
	// the UI tails it. It lives on the leader and is ephemeral. The report API
	// writes to it, and the UI reads from it.
	liveLogs := ci.NewLiveLogs()
	reportAPI := ci.NewReportAPI(md, objs, liveLogs, md.IsLeader, workerSecret, logger)
	writer.AfterCommit = func(ctx context.Context, pusherID int64, commits []githttp.CommitInfo) {
		for _, c := range commits {
			dispatcher.Dispatch(ctx, ci.Event{
				RepoID:   c.RepoID,
				RepoName: c.RepoName,
				Ref:      c.Ref,
				OldSHA:   c.Old,
				NewSHA:   c.New,
				PusherID: pusherID,
			})
		}
	}
	cleanup := func() error {
		err := writer.Close()
		// On a clean shutdown, Close durably flushes replication, so a redeploy or
		// restart loses nothing. Without a replica, the flush is a no-op. Bound the
		// flush, so an unreachable S3 can't hang shutdown. The accepted crash-loss
		// window still covers a hard kill (docs/architecture/safety.md, section 4).
		syncCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if cerr := md.CloseContext(syncCtx); err == nil {
			err = cerr
		}
		return err
	}

	handler, err := githttp.New(githttp.Config{
		Materializer: materializer,
		Authorizer:   guard,
		Writer:       writer,
		IsWritable:   md.IsLeader,
		Logger:       logger,
	})
	if err != nil {
		_ = cleanup()
		return nil, nil, nil, nil, noop, err
	}

	ui, err := buildUI(ctx, md, materializer, objs, liveLogs, guard, secretsSvc, logger)
	if err != nil {
		_ = cleanup()
		return nil, nil, nil, nil, noop, err
	}

	// Hand the lease back whenever this writer is idle. The loop stops before the
	// metadata closes, so a yield never runs at the same time as the final Close.
	yieldCtx, stopYield := context.WithCancel(ctx)
	yieldDone := make(chan struct{})
	go func() {
		defer close(yieldDone)
		yieldWhenIdle(yieldCtx, act, md, logger)
	}()
	closeAll := func() error {
		stopYield()
		<-yieldDone
		return cleanup()
	}
	return handler, ui, reportAPI, gate, closeAll, nil
}

// buildUI constructs the management UI. Its session cookie
// key is GITMOTE_COOKIE_KEY when that is set. Otherwise, it is an auto-generated
// key, persisted in meta and restored across a restart or a scale-to-zero. So a
// session stays valid when an idle instance wakes, and the UI needs no secret
// config.
func buildUI(ctx context.Context, md *meta.Metadata, mz *repo.Materializer, objs store.Store, live webui.LiveLog, guard *auth.Guard, secretsSvc *secrets.Service, logger *slog.Logger) (*webui.Handler, error) {
	cookieKey, err := resolveCookieKey(ctx, md)
	if err != nil {
		return nil, fmt.Errorf("cookie key: %w", err)
	}
	return webui.New(md, mz, objs, live, guard, secretsSvc, cookieKey, logger)
}

// resolveCookieKey returns the session cookie key. That is GITMOTE_COOKIE_KEY
// when it is set. Otherwise, it is the persisted "cookie_key" server secret,
// which is auto-generated on first boot.
func resolveCookieKey(ctx context.Context, md *meta.Metadata) ([]byte, error) {
	if v := os.Getenv("GITMOTE_COOKIE_KEY"); v != "" {
		return []byte(v), nil
	}
	return md.GetOrCreateSecret(ctx, "cookie_key", genSecret)
}

// resolveWorkerSecret returns the CI report-API secret. That is WORKER_SECRET
// when it is set. Otherwise, it is the persisted "worker_secret" server secret,
// which is auto-generated on first boot.
func resolveWorkerSecret(ctx context.Context, md *meta.Metadata) (string, error) {
	if v := os.Getenv("WORKER_SECRET"); v != "" {
		return v, nil
	}
	b, err := md.GetOrCreateSecret(ctx, "worker_secret", genSecret)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// genSecretBytes is the entropy of an auto-generated server secret before
// hex encoding. 32 bytes give a 64-char hex string. That is ample for an HMAC
// cookie key and for the bearer comparison of the worker secret.
const genSecretBytes = 32

// genSecret generates a fresh server secret as the bytes of a hex string. So the
// persisted value is printable, and it works directly as an HMAC key or a bearer
// token. It is the get-or-create generator for both auto-provisioned secrets.
func genSecret() ([]byte, error) {
	b := make([]byte, genSecretBytes)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	enc := make([]byte, hex.EncodedLen(len(b)))
	hex.Encode(enc, b)
	return enc, nil
}

// hookBinaryPath is the pre-receive hook executable, gitmote-hook, beside this
// binary. runnerBinaryPath is the CI runner that the local trigger spawns.
func hookBinaryPath() string   { return besideExe("gitmote-hook") }
func runnerBinaryPath() string { return besideExe("gitmote-runner") }

// besideExe resolves a sibling binary next to this executable. When the
// executable path can't be determined, it falls back to the bare name, which is
// then found on PATH.
func besideExe(name string) string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), name)
	}
	return name
}

// metaConfigFromEnv builds the metadata database config from the environment.
// The replica (see replicaTarget) is an s3:// URL. It serves both the restore on
// cold start and the continuous backup (litestream). An empty replica leaves the
// database local-only, as in tests and ephemeral runs with no bucket. So the env
// alone decides whether the same binary runs replicated.
//
// The replica reuses the object store's endpoint and the AWS default credential
// chain. So one set of credentials covers both the git objects and the metadata
// WAL.
//
// role selects single-writer coordination. It applies only when a replica is
// configured, because without a shared WAL there is nothing to coordinate on.
// So the role of an unreplicated database stays unset (RoleAuto), and the
// database runs as the sole writer. Tests therefore behave the same. An unset
// role also avoids handing s3lite a coordinating role with no s3:// replica to
// lease. The RoleWriter of bootstrap is such a role.
func metaConfigFromEnv(logger *slog.Logger, role s3lite.Role) meta.Config {
	replica := replicaTarget()
	cfg := meta.Config{
		LocalPath:   dbPath(),
		RestoreFrom: replica,
		BackupTo:    replica,
		S3:          s3lite.S3Config{Endpoint: os.Getenv("GITMOTE_S3_ENDPOINT")},
		Logger:      logger,
	}
	if replica != "" {
		cfg.Role = role
		// A follower promotes only through the request gate, never in the
		// background. So a lease that an idle writer yields stays free for the
		// instance that gets the next request.
		cfg.OnDemandPromotion = true
	}
	return cfg
}

// replicaTarget derives the metadata replica URL from the storage root. The
// metadata lives at {root}/meta, a sibling of the git objects under the same
// root. So a bucket alone gives durability and the single-writer lease, with no
// second env.
//
// The replica is under the base, not the bare bucket, on purpose. Refs and
// objects then share fate. A wrong root restores nothing. It never leaves refs
// that still advertise objects that are missing.
func replicaTarget() string {
	bucket, prefix := store.ParseBucket(os.Getenv("GITMOTE_S3_BUCKET"))
	if bucket == "" {
		return ""
	}
	root := bucket
	if prefix != "" {
		root += "/" + prefix
	}
	return "s3://" + root + "/meta"
}

// dataDir is the base directory for gitmote's local state, which restores from
// S3. It is GITMOTE_DATA, with a temp dir as the default. The db, cache, and
// socket all live under it. So one -v ./data:/data mounts all of them.
func dataDir() string {
	return envOr("GITMOTE_DATA", filepath.Join(os.TempDir(), "gitmote"))
}

// dbPath, cachePath, and sockPath are paths under dataDir:
//   - dbPath is the metadata DB.
//   - cachePath is the cache of materialized repos.
//   - sockPath is the pre-receive hook socket.
func dbPath() string    { return filepath.Join(dataDir(), "meta.sqlite3") }
func cachePath() string { return filepath.Join(dataDir(), "cache") }
func sockPath() string  { return filepath.Join(dataDir(), "gitmote.sock") }

// envOr returns the value of the environment variable key, or fallback if
// it is unset or empty.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
