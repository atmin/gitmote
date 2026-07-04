// Command gitmote is the gitmote server binary.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
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

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

const shutdownTimeout = 10 * time.Second

const (
	// reconcileInterval is how often the leader sweeps abandoned CI jobs.
	reconcileInterval = 5 * time.Minute
	// reconcileMaxAge bounds a running job: past this, a runner is presumed dead
	// and its job is failed. It matches the clone-token TTL — a job that outlives
	// its credential can't finish anyway.
	reconcileMaxAge = time.Hour
)

func main() {
	// Subcommands run without the server (single-writer admin, per
	// docs/notes/bootstrap.md); the default (no subcommand) is the server.
	if args := os.Args[1:]; len(args) > 0 && args[0] == "bootstrap" {
		if err := runBootstrap(context.Background(), args[1:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "bootstrap:", err)
			os.Exit(1)
		}
		return
	}

	addr := flag.String("addr", envOr("GITMOTE_ADDR", ":8080"), "listen address")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	if err := run(context.Background(), logger, *addr); err != nil {
		logger.Error("server failed", "error", err)
		os.Exit(1)
	}
}

// runBootstrap creates the first admin, token, and repo from an empty instance.
// It opens the metadata DB per the environment (GITMOTE_DB, and GITMOTE_DB_REPLICA
// for litestream) and prints the one-time token to out on success. The deferred
// Close durably flushes replication, so this short-lived process reliably pushes
// the new admin/token/repo to S3 for the server to restore.
func runBootstrap(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	handle := fs.String("handle", os.Getenv("GITMOTE_ADMIN_HANDLE"), "admin user handle (or GITMOTE_ADMIN_HANDLE)")
	repoName := fs.String("repo", "", "initial repository, e.g. atmin/gitmote")
	branch := fs.String("default-branch", "main", "default branch for the initial repo")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Bootstrap is a deliberate single-shot writer: RoleWriter acquires the lease
	// (when a replica is configured) and fails loudly if a live server already
	// holds it, so bootstrapping can never race the running writer.
	md, err := meta.Open(ctx, metaConfigFromEnv(nil, s3lite.RoleWriter))
	if err != nil {
		return fmt.Errorf("open metadata: %w", err)
	}
	defer func() { _ = md.Close() }()

	res, err := bootstrap.Run(ctx, md, bootstrap.Options{
		AdminHandle:   *handle,
		RepoName:      *repoName,
		DefaultBranch: *branch,
	})
	if err != nil {
		return err
	}

	if res.AlreadyBootstrapped {
		_, err := io.WriteString(out, "already bootstrapped: an admin exists; nothing to do\n")
		return err
	}

	_, err = fmt.Fprintf(out,
		"admin user:   %s\n"+
			"initial repo: %s\n\n"+
			"access token (shown once — save it now):\n\n    %s\n\n"+
			"clone/push with:  git clone http://%s:<token>@<host>/%s\n",
		res.Admin.Handle, res.Repo.Name, res.RawToken, res.Admin.Handle, res.Repo.Name)
	return err
}

// run starts the HTTP server and blocks until ctx is cancelled or a
// SIGINT/SIGTERM arrives, then shuts the server down gracefully.
func run(ctx context.Context, logger *slog.Logger, addr string) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	gitHandler, ui, reportAPI, isLeader, closeMeta, err := buildGitHandler(ctx, logger)
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
		Handler: newHandler(gitHandler, ui, reportAPI, isLeader),
	}

	if reportAPI != nil {
		go reconcileLoop(ctx, reportAPI, logger)
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

// reconcileLoop periodically sweeps abandoned CI jobs (a runner that died mid-run
// leaves a job stuck in running). ReconcileStuck is leader-gated internally, so a
// follower's ticks are no-ops. It stops when ctx is cancelled (shutdown).
func reconcileLoop(ctx context.Context, reportAPI *ci.ReportAPI, logger *slog.Logger) {
	t := time.NewTicker(reconcileInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := reportAPI.ReconcileStuck(ctx, reconcileMaxAge, time.Now()); err != nil {
				logger.Error("ci: reconcile sweep failed", "error", err)
			}
		}
	}
}

// newHandler returns the server's HTTP routes. ui, when non-nil, registers the
// management UI under /ui/ and /login (more specific patterns that win over the
// git catch-all). gitHandler, when non-nil, serves the git smart-HTTP endpoints
// at "/"; the exact health/version routes stay more specific.
//
// isLeader gates every metadata-derived response on the writer lease (see
// leaderGate): a follower — the brief post-deploy window before it promotes —
// serves a frozen, stale snapshot, so it must not answer with stale refs.
func newHandler(gitHandler http.Handler, ui *webui.Handler, reportAPI *ci.ReportAPI, isLeader func() bool) http.Handler {
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
	return leaderGate(mux, isLeader)
}

// leaderGate serves stale-free: only the writer (leader) answers requests that
// read or write the metadata DB. A follower restored a snapshot at startup and
// does not catch up until it promotes (s3lite RoleAuto), so its refs can lag the
// true tip — a browse would show a just-pushed file as missing, a fetch would
// miss commits. Rather than serve that, a follower returns 503 + Retry-After.
//
// Exceptions that must stay up on a follower: the liveness probes (Scaleway's
// health check — gating them would deadlock a rolling deploy, since the new
// instance can't promote until the old one drains) and static assets (no
// metadata). A nil isLeader (unreplicated dev, RoleOff) means always-leader.
func leaderGate(next http.Handler, isLeader func() bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if alwaysServable(r.URL.Path) || isLeader == nil || isLeader() {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Retry-After", "2")
		http.Error(w, "gitmote is warming up (not the writer yet) — retry shortly",
			http.StatusServiceUnavailable)
	})
}

// alwaysServable reports whether a path is answerable by a follower: the liveness
// probes and static assets, which carry no metadata.
func alwaysServable(p string) bool {
	return p == "/healthz" || p == "/version" || strings.HasPrefix(p, "/ui/static/")
}

// buildGitHandler wires the git read + write paths when an object store is
// configured (GITMOTE_S3_BUCKET). Without it, the server runs health/version
// only — a dev convenience — and the returned close func is a no-op.
//
// The management UI (second return) is built when GITMOTE_COOKIE_KEY is set (the
// HMAC key that signs session cookies); it shares this metadata handle, so it
// runs alongside the git server.
//
// Metadata lives at GITMOTE_DB (default ./gitmote.sqlite3), replicated to
// GITMOTE_DB_REPLICA when set (litestream; empty is local-only). Materialized
// repos cache under GITMOTE_CACHE (default a temp dir). The push path listens on
// GITMOTE_SOCK (default a temp path) and installs the pre-receive hook binary
// at GITMOTE_HOOK (default gitmote-hook alongside this binary).
func buildGitHandler(ctx context.Context, logger *slog.Logger) (http.Handler, *webui.Handler, *ci.ReportAPI, func() bool, func() error, error) {
	noop := func() error { return nil }

	if os.Getenv("GITMOTE_S3_BUCKET") == "" {
		logger.Warn("GITMOTE_S3_BUCKET unset; git endpoints disabled (health/version only)")
		return nil, nil, nil, nil, noop, nil
	}

	objs, err := store.NewS3FromEnv(ctx)
	if err != nil {
		return nil, nil, nil, nil, noop, fmt.Errorf("object store: %w", err)
	}

	// RoleAuto: this instance becomes the writer when it can acquire the lease,
	// otherwise a read-only follower — so a rolling deploy's brief old+new overlap
	// is one writer + one follower, never two writers (safety.md §1). With no
	// replica configured this is RoleOff (always writer): tests and local dev
	// unchanged.
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

	sockPath := envOr("GITMOTE_SOCK", filepath.Join(os.TempDir(), "gitmote.sock"))
	writer, err := githttp.NewWriter(md, objs, hookBinaryPath(), sockPath, logger)
	if err != nil {
		_ = md.Close()
		return nil, nil, nil, nil, noop, fmt.Errorf("write path: %w", err)
	}
	cacheRoot := envOr("GITMOTE_CACHE", filepath.Join(os.TempDir(), "gitmote-repos"))
	materializer := repo.New(md, objs, cacheRoot)
	guard := auth.NewGuard(md)

	// CI secrets: master keys from GITMOTE_CI_SECRET_KEY_V<n>. A malformed key is
	// fatal (fail loud). With none set the service is disabled — no secrets UI,
	// none injected. The service is the single hub for the UI and the dispatcher.
	keyring, err := secrets.NewKeyringFromEnv()
	if err != nil {
		_ = writer.Close()
		_ = md.Close()
		return nil, nil, nil, nil, noop, fmt.Errorf("ci secrets: %w", err)
	}
	secretsSvc := secrets.NewService(keyring, md)

	// Select the CI trigger. Cloud and local run the *same* runner code and env
	// contract; only the substrate differs — Scaleway Serverless Jobs in
	// production, a local process for dev (tasks/16-ci.md, tasks/21-ci-runner.md).
	// Both need a reachable GITMOTE_URL (the runner clones + reports back over it)
	// and a WORKER_SECRET (authenticates the report API). With neither configured,
	// runs still record but nothing executes.
	jobDefID := os.Getenv("SCW_CI_JOB_DEFINITION_ID")
	workerSecret := os.Getenv("WORKER_SECRET")
	gitmoteURL := os.Getenv("GITMOTE_URL")
	var trigger ci.Trigger = ci.NoopTrigger{}
	switch {
	case jobDefID != "":
		if workerSecret == "" || gitmoteURL == "" {
			_ = writer.Close()
			_ = md.Close()
			return nil, nil, nil, nil, noop, fmt.Errorf("SCW_CI_JOB_DEFINITION_ID is set but WORKER_SECRET or GITMOTE_URL is missing")
		}
		// SCW_REGION falls back to the AWS_REGION already set for the object store.
		trigger = scaleway.NewJobsClient(os.Getenv("SCW_SECRET_KEY"), envOr("SCW_REGION", os.Getenv("AWS_REGION")), jobDefID)
		logger.Info("CI trigger: Scaleway Serverless Jobs", "job_definition_id", jobDefID)
	case workerSecret != "" && gitmoteURL != "":
		trigger = ci.NewLocalTrigger(runnerBinaryPath(), logger)
		logger.Info("CI trigger: local runner", "runner", runnerBinaryPath(), "url", gitmoteURL)
	default:
		logger.Warn("CI trigger disabled; runs record but do not execute " +
			"(set GITMOTE_URL + WORKER_SECRET for local, or SCW_CI_JOB_DEFINITION_ID for cloud)")
	}

	// A successful, branch-advancing push discovers its workflows, records a run,
	// and triggers a runner per job — fire-and-forget, so dispatch never fails the
	// push (tasks/16-ci.md).
	dispatcher := ci.NewDispatcher(ci.Config{
		Runs:         md,
		Materializer: materializer,
		Trigger:      trigger,
		Minter:       guard,
		Secrets:      secretsSvc,
		BaseURL:      gitmoteURL,
		WorkerSecret: workerSecret,
		Logger:       logger,
	})
	// The runner's authenticated claim/complete API. Only the leader may write
	// completions (a follower returns a retryable 503), and it authenticates with
	// the same WORKER_SECRET injected into the runner env.
	reportAPI := ci.NewReportAPI(md, objs, md.IsLeader, workerSecret, logger)
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
		// Close durably flushes replication on clean shutdown so a redeploy/restart
		// is lossless (a no-op without a replica). Bound the flush so an unreachable
		// S3 can't hang shutdown; the accepted crash-loss window (safety.md §4)
		// still covers a hard kill.
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

	ui, err := buildUI(md, materializer, objs, guard, secretsSvc, logger)
	if err != nil {
		_ = cleanup()
		return nil, nil, nil, nil, noop, err
	}
	return handler, ui, reportAPI, md.IsLeader, cleanup, nil
}

// buildUI constructs the management UI when GITMOTE_COOKIE_KEY is set; otherwise
// it returns nil (UI disabled) so a misconfigured key never yields an insecurely
// signed session.
func buildUI(md *meta.Metadata, mz *repo.Materializer, objs store.Store, guard *auth.Guard, secretsSvc *secrets.Service, logger *slog.Logger) (*webui.Handler, error) {
	key := os.Getenv("GITMOTE_COOKIE_KEY")
	if key == "" {
		logger.Warn("GITMOTE_COOKIE_KEY unset; management UI disabled")
		return nil, nil
	}
	return webui.New(md, mz, objs, guard, secretsSvc, []byte(key), logger)
}

// hookBinaryPath resolves the pre-receive hook executable: GITMOTE_HOOK if set,
// otherwise gitmote-hook next to this binary.
func hookBinaryPath() string {
	if p := os.Getenv("GITMOTE_HOOK"); p != "" {
		return p
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "gitmote-hook")
	}
	return "gitmote-hook"
}

// runnerBinaryPath resolves the CI runner executable the local trigger spawns:
// GITMOTE_RUNNER if set, otherwise gitmote-runner next to this binary.
func runnerBinaryPath() string {
	if p := os.Getenv("GITMOTE_RUNNER"); p != "" {
		return p
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "gitmote-runner")
	}
	return "gitmote-runner"
}

// metaConfigFromEnv builds the metadata database config from the environment.
// GITMOTE_DB_REPLICA is an s3:// URL used for both restore-on-cold-start and
// continuous backup (litestream); empty leaves the database local-only (tests,
// local dev), so the same binary runs unreplicated or replicated by env alone.
// The replica reuses the object store's endpoint and the AWS default credential
// chain, so one credential set covers both git objects and the metadata WAL.
//
// role selects single-writer coordination and is applied only when a replica is
// configured — there is nothing to coordinate on without a shared WAL, so an
// unreplicated database stays RoleOff (always writer), keeping tests and local
// dev unchanged.
func metaConfigFromEnv(logger *slog.Logger, role s3lite.Role) meta.Config {
	replica := os.Getenv("GITMOTE_DB_REPLICA")
	cfg := meta.Config{
		LocalPath:   envOr("GITMOTE_DB", "gitmote.sqlite3"),
		RestoreFrom: replica,
		BackupTo:    replica,
		S3:          s3lite.S3Config{Endpoint: os.Getenv("GITMOTE_S3_ENDPOINT")},
		Logger:      logger,
	}
	if replica != "" {
		cfg.Role = role
	}
	return cfg
}

// envOr returns the value of the environment variable key, or fallback if
// it is unset or empty.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
