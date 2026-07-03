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
	"syscall"
	"time"

	"github.com/atmin/gitmote/internal/auth"
	"github.com/atmin/gitmote/internal/bootstrap"
	"github.com/atmin/gitmote/internal/githttp"
	"github.com/atmin/gitmote/internal/meta"
	"github.com/atmin/gitmote/internal/repo"
	"github.com/atmin/gitmote/internal/store"
	"github.com/atmin/gitmote/internal/webui"
	"github.com/atmin/s3lite"
)

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

const shutdownTimeout = 10 * time.Second

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

	gitHandler, ui, closeMeta, err := buildGitHandler(ctx, logger)
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
		Handler: newHandler(gitHandler, ui),
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

// newHandler returns the server's HTTP routes. ui, when non-nil, registers the
// management UI under /ui/ and /login (more specific patterns that win over the
// git catch-all). gitHandler, when non-nil, serves the git smart-HTTP endpoints
// at "/"; the exact health/version routes stay more specific.
func newHandler(gitHandler http.Handler, ui *webui.Handler) http.Handler {
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
	if gitHandler != nil {
		mux.Handle("/", gitHandler)
	}
	return mux
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
func buildGitHandler(ctx context.Context, logger *slog.Logger) (http.Handler, *webui.Handler, func() error, error) {
	noop := func() error { return nil }

	if os.Getenv("GITMOTE_S3_BUCKET") == "" {
		logger.Warn("GITMOTE_S3_BUCKET unset; git endpoints disabled (health/version only)")
		return nil, nil, noop, nil
	}

	objs, err := store.NewS3FromEnv(ctx)
	if err != nil {
		return nil, nil, noop, fmt.Errorf("object store: %w", err)
	}

	// RoleAuto: this instance becomes the writer when it can acquire the lease,
	// otherwise a read-only follower — so a rolling deploy's brief old+new overlap
	// is one writer + one follower, never two writers (safety.md §1). With no
	// replica configured this is RoleOff (always writer): tests and local dev
	// unchanged.
	md, err := meta.Open(ctx, metaConfigFromEnv(logger, s3lite.RoleAuto))
	if err != nil {
		return nil, nil, noop, fmt.Errorf("metadata: %w", err)
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
		return nil, nil, noop, fmt.Errorf("write path: %w", err)
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

	guard := auth.NewGuard(md)
	cacheRoot := envOr("GITMOTE_CACHE", filepath.Join(os.TempDir(), "gitmote-repos"))
	handler, err := githttp.New(githttp.Config{
		Materializer: repo.New(md, objs, cacheRoot),
		Authorizer:   guard,
		Writer:       writer,
		IsWritable:   md.IsLeader,
		Logger:       logger,
	})
	if err != nil {
		_ = cleanup()
		return nil, nil, noop, err
	}

	ui, err := buildUI(md, guard, logger)
	if err != nil {
		_ = cleanup()
		return nil, nil, noop, err
	}
	return handler, ui, cleanup, nil
}

// buildUI constructs the management UI when GITMOTE_COOKIE_KEY is set; otherwise
// it returns nil (UI disabled) so a misconfigured key never yields an insecurely
// signed session.
func buildUI(md *meta.Metadata, guard *auth.Guard, logger *slog.Logger) (*webui.Handler, error) {
	key := os.Getenv("GITMOTE_COOKIE_KEY")
	if key == "" {
		logger.Warn("GITMOTE_COOKIE_KEY unset; management UI disabled")
		return nil, nil
	}
	return webui.New(md, guard, []byte(key), logger)
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
