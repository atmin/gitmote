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
// It opens the metadata DB at GITMOTE_DB (local-only; litestream is wired in the
// deploy task) and prints the one-time token to out on success.
func runBootstrap(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	handle := fs.String("handle", os.Getenv("GITMOTE_ADMIN_HANDLE"), "admin user handle (or GITMOTE_ADMIN_HANDLE)")
	repoName := fs.String("repo", "", "initial repository, e.g. atmin/gitmote")
	branch := fs.String("default-branch", "main", "default branch for the initial repo")
	if err := fs.Parse(args); err != nil {
		return err
	}

	md, err := meta.Open(ctx, meta.Config{LocalPath: envOr("GITMOTE_DB", "gitmote.sqlite3")})
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

	gitHandler, closeMeta, err := buildGitHandler(ctx, logger)
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
		Handler: newHandler(gitHandler),
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

// newHandler returns the server's HTTP routes. gitHandler, when non-nil, serves
// the git smart-HTTP endpoints at the catch-all "/"; the exact health/version
// routes stay more specific and win under http.ServeMux.
func newHandler(gitHandler http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, version)
	})
	if gitHandler != nil {
		mux.Handle("/", gitHandler)
	}
	return mux
}

// buildGitHandler wires the git read + write paths when an object store is
// configured (GITMOTE_S3_BUCKET). Without it, the server runs health/version
// only — a dev convenience — and the returned close func is a no-op.
//
// Metadata lives at GITMOTE_DB (default ./gitmote.sqlite3); materialized repos
// cache under GITMOTE_CACHE (default a temp dir). The push path listens on
// GITMOTE_SOCK (default a temp path) and installs the pre-receive hook binary
// at GITMOTE_HOOK (default gitmote-hook alongside this binary). Litestream
// replication of the metadata DB is wired in the deploy task (11); it is
// local-only here.
func buildGitHandler(ctx context.Context, logger *slog.Logger) (http.Handler, func() error, error) {
	noop := func() error { return nil }

	if os.Getenv("GITMOTE_S3_BUCKET") == "" {
		logger.Warn("GITMOTE_S3_BUCKET unset; git endpoints disabled (health/version only)")
		return nil, noop, nil
	}

	objs, err := store.NewS3FromEnv(ctx)
	if err != nil {
		return nil, noop, fmt.Errorf("object store: %w", err)
	}

	dbPath := envOr("GITMOTE_DB", "gitmote.sqlite3")
	md, err := meta.Open(ctx, meta.Config{LocalPath: dbPath})
	if err != nil {
		return nil, noop, fmt.Errorf("metadata: %w", err)
	}

	sockPath := envOr("GITMOTE_SOCK", filepath.Join(os.TempDir(), "gitmote.sock"))
	writer, err := githttp.NewWriter(md, objs, hookBinaryPath(), sockPath, logger)
	if err != nil {
		_ = md.Close()
		return nil, noop, fmt.Errorf("write path: %w", err)
	}
	cleanup := func() error {
		err := writer.Close()
		if cerr := md.Close(); err == nil {
			err = cerr
		}
		return err
	}

	cacheRoot := envOr("GITMOTE_CACHE", filepath.Join(os.TempDir(), "gitmote-repos"))
	handler, err := githttp.New(githttp.Config{
		Materializer: repo.New(md, objs, cacheRoot),
		Authorizer:   auth.NewGuard(md),
		Writer:       writer,
		Logger:       logger,
	})
	if err != nil {
		_ = cleanup()
		return nil, noop, err
	}
	return handler, cleanup, nil
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

// envOr returns the value of the environment variable key, or fallback if
// it is unset or empty.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
