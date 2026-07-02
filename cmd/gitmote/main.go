// Command gitmote is the gitmote server binary.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/atmin/gitmote/internal/githttp"
	"github.com/atmin/gitmote/internal/meta"
	"github.com/atmin/gitmote/internal/repo"
	"github.com/atmin/gitmote/internal/store"
)

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

const shutdownTimeout = 10 * time.Second

func main() {
	addr := flag.String("addr", envOr("GITMOTE_ADDR", ":8080"), "listen address")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	if err := run(context.Background(), logger, *addr); err != nil {
		logger.Error("server failed", "error", err)
		os.Exit(1)
	}
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

// buildGitHandler wires the git read path (task 05) when an object store is
// configured (GITMOTE_S3_BUCKET). Without it, the server runs health/version
// only — a dev convenience — and the returned close func is a no-op.
//
// Metadata lives at GITMOTE_DB (default ./gitmote.sqlite3); materialized repos
// cache under GITMOTE_CACHE (default a temp dir). Litestream replication of the
// metadata DB is wired in the deploy task (11); it is local-only here.
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

	cacheRoot := envOr("GITMOTE_CACHE", filepath.Join(os.TempDir(), "gitmote-repos"))
	handler, err := githttp.New(repo.New(md, objs, cacheRoot), logger)
	if err != nil {
		_ = md.Close()
		return nil, noop, err
	}
	return handler, md.Close, nil
}

// envOr returns the value of the environment variable key, or fallback if
// it is unset or empty.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
