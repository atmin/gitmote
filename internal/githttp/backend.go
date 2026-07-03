// Package githttp serves git smart-HTTP. It materializes the target repo (refs
// from s3lite, objects hydrated from the object store) and delegates protocol
// work — ref advertisement, pack negotiation, receive-pack — to stock
// `git http-backend` over CGI, per the request flows in
// docs/architecture/request-flows.md.
//
// The read path (clone / fetch, upload-pack) needs only a materialized repo.
// The write path (push, receive-pack) additionally serializes per repo, mints a
// per-push nonce, and installs the pre-receive hook that RPCs the parent to
// enforce the content-before-pointer CAS; that machinery lives in the Writer
// (receive.go) and is engaged only when the handler is built with one.
package githttp

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"strings"

	"github.com/atmin/gitmote/internal/auth"
	"github.com/atmin/gitmote/internal/meta"
	"github.com/atmin/gitmote/internal/repo"
)

// Authorizer guards a request before it is served: it authenticates the PAT and
// checks the user holds perm on repoName, returning auth's error sentinels
// (ErrUnauthorized / ErrForbidden), meta.ErrNotFound, or an internal error.
type Authorizer interface {
	Authorize(r *http.Request, repoName string, perm meta.Perm) (*meta.User, error)
}

// Config assembles a Handler.
type Config struct {
	// Materializer builds the on-disk repo git-http-backend serves. Required.
	Materializer *repo.Materializer
	// Authorizer guards every request. Required.
	Authorizer Authorizer
	// Writer enables the push path. Nil serves the read path only; write
	// endpoints then 404.
	Writer *Writer
	// IsWritable reports whether this instance may serve writes — it holds the
	// litestream lease (see meta.Metadata.IsLeader). Nil means always writable
	// (unreplicated / tests). A follower refuses receive-pack with a retryable
	// 503; reads are unaffected.
	IsWritable func() bool
	// Logger is optional; nil discards.
	Logger *slog.Logger
}

// Handler serves the git smart-HTTP endpoints for any repo, dispatching on the
// git URL suffix. Mount it at "/"; more specific routes (e.g. /healthz) still
// win under http.ServeMux.
type Handler struct {
	mz         *repo.Materializer
	authz      Authorizer
	writer     *Writer
	isWritable func() bool
	gitPath    string
	logger     *slog.Logger
}

// New returns a handler. It fails if the `git` executable is not on PATH, since
// the whole design delegates to it.
func New(cfg Config) (*Handler, error) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("githttp: git not found on PATH: %w", err)
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Handler{
		mz:         cfg.Materializer,
		authz:      cfg.Authorizer,
		writer:     cfg.Writer,
		isWritable: cfg.IsWritable,
		gitPath:    gitPath,
		logger:     logger,
	}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	repoName, endpoint, ok := parseGitPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	perm, isReceive, isPush, ok := classify(endpoint, r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	// Writes are served only when a Writer is configured.
	if isReceive && h.writer == nil {
		http.NotFound(w, r)
		return
	}
	// Only the lease-holding writer serves receive-pack; a read-only follower
	// refuses pushes (safety.md §1) with a retryable 503 so the client can try
	// again once the leader is up. This gates both the advertisement and the
	// push POST. Reads (upload-pack) are served in either role.
	if isReceive && h.isWritable != nil && !h.isWritable() {
		w.Header().Set("Retry-After", "5")
		http.Error(w, "gitmote: not the current writer, retry shortly", http.StatusServiceUnavailable)
		return
	}

	if _, err := h.authz.Authorize(r, repoName, perm); err != nil {
		h.writeAuthError(w, r, repoName, err)
		return
	}

	if isPush {
		h.serveReceivePack(w, r, repoName)
		return
	}

	// Read, or a receive-pack advertisement: materialize then hand off to
	// http-backend. The receive advertisement needs http.receivepack enabled.
	if !h.materialize(w, r, repoName) {
		return
	}
	var extra []string
	if isReceive {
		extra = gitConfigEnv([2]string{"http.receivepack", "true"})
	}
	h.serveCGI(w, r, extra)
}

// serveReceivePack runs a push under the per-repo write lock: refresh the repo
// from the sources of truth, mint a nonce, and hand off to receive-pack with the
// pre-receive hook wired to call back on the socket. The hook's callback does
// the object PUT + ref CAS (see Writer.handle).
func (h *Handler) serveReceivePack(w http.ResponseWriter, r *http.Request, repoName string) {
	push, err := h.writer.Begin(r.Context(), repoName)
	if err != nil {
		if errors.Is(err, meta.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		h.logger.Error("begin push failed", "repo", repoName, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer push.Release()

	// Materialize under the lock so receive-pack's fast-forward check runs
	// against the authoritative tip.
	if !h.materialize(w, r, repoName) {
		return
	}

	extra := append(
		gitConfigEnv(
			[2]string{"http.receivepack", "true"},
			[2]string{"core.hooksPath", h.writer.HooksPath()},
		),
		"GITMOTE_SOCK="+h.writer.SockPath(),
		"GITMOTE_NONCE="+push.Nonce,
	)
	h.serveCGI(w, r, extra)
}

// materialize builds the on-disk repo, writing the HTTP error itself on failure
// and returning false so the caller stops.
func (h *Handler) materialize(w http.ResponseWriter, r *http.Request, repoName string) bool {
	if _, err := h.mz.Materialize(r.Context(), repoName); err != nil {
		if errors.Is(err, meta.ErrNotFound) {
			http.NotFound(w, r)
			return false
		}
		h.logger.Error("materialize failed", "repo", repoName, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return false
	}
	return true
}

// serveCGI delegates to git-http-backend. PATH_INFO carries the full request
// path (empty CGI Root) and GIT_PROJECT_ROOT is the cache root, so http-backend
// resolves the repo dir the materializer just wrote.
func (h *Handler) serveCGI(w http.ResponseWriter, r *http.Request, extraEnv []string) {
	// git http-backend runs via CGI and needs CONTENT_LENGTH. A request with an
	// unknown length — chunked transfer-encoding, which git uses for a pack larger
	// than the client's http.postBuffer (default 1 MiB) — carries none, and
	// http-backend rejects it with 400. Buffer such a body to a temp file to give
	// it a fixed length before handing off, so a large push works with no client
	// http.postBuffer tuning. Bodies with a known length stream through unchanged.
	if r.Body != nil && r.ContentLength < 0 {
		cleanup, err := bufferBodyToFile(r)
		if err != nil {
			h.logger.Error("buffering request body", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		defer cleanup()
	}

	env := append([]string{
		"GIT_PROJECT_ROOT=" + h.mz.Root(),
		"GIT_HTTP_EXPORT_ALL=1",
	}, extraEnv...)
	backend := &cgi.Handler{
		Path: h.gitPath,
		Args: []string{"http-backend"},
		Dir:  h.mz.Root(),
		Env:  env,
	}
	backend.ServeHTTP(w, r)
}

// bufferBodyToFile drains r.Body to a temp file and rewires the request to read
// from it with a known ContentLength, turning a length-unknown (chunked) body
// into a fixed-length one for the CGI child. It returns a cleanup that closes and
// removes the file after the response is served. Spilling to disk (not memory)
// keeps a large pack off the heap.
func bufferBodyToFile(r *http.Request) (func(), error) {
	f, err := os.CreateTemp("", "gitmote-body-*")
	if err != nil {
		return nil, err
	}
	cleanup := func() { _ = f.Close(); _ = os.Remove(f.Name()) }
	n, err := io.Copy(f, r.Body)
	_ = r.Body.Close()
	if err != nil {
		cleanup()
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, err
	}
	r.Body = f
	r.ContentLength = n
	r.TransferEncoding = nil
	return cleanup, nil
}

// writeAuthError maps an Authorize error to the HTTP response git expects: a
// 401 carries the Basic challenge so git (and its credential helpers) retry
// with credentials; a 403 is a hard stop; a missing repo is a 404.
func (h *Handler) writeAuthError(w http.ResponseWriter, r *http.Request, repoName string, err error) {
	switch {
	case errors.Is(err, auth.ErrForbidden):
		http.Error(w, "forbidden", http.StatusForbidden)
	case errors.Is(err, auth.ErrUnauthorized):
		w.Header().Set("WWW-Authenticate", `Basic realm="gitmote"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	case errors.Is(err, meta.ErrNotFound):
		http.NotFound(w, r)
	default:
		h.logger.Error("authorize failed", "repo", repoName, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// classify determines, for a parsed endpoint and request, the required
// permission and whether it targets receive-pack (isReceive) and mutates
// (isPush). ok=false rejects a wrong method or unknown service.
func classify(endpoint string, r *http.Request) (perm meta.Perm, isReceive, isPush, ok bool) {
	switch endpoint {
	case "info/refs":
		if r.Method != http.MethodGet {
			return "", false, false, false
		}
		switch r.URL.Query().Get("service") {
		case "git-upload-pack":
			return meta.PermRead, false, false, true
		case "git-receive-pack":
			return meta.PermWrite, true, false, true
		default:
			return "", false, false, false
		}
	case "git-upload-pack":
		if r.Method != http.MethodPost {
			return "", false, false, false
		}
		return meta.PermRead, false, false, true
	case "git-receive-pack":
		if r.Method != http.MethodPost {
			return "", false, false, false
		}
		return meta.PermWrite, true, true, true
	default:
		return "", false, false, false
	}
}

// parseGitPath splits a git smart-HTTP URL into the repo name and the endpoint
// suffix ("info/refs", "git-upload-pack", or "git-receive-pack"). The repo name
// — which may contain slashes ("atmin/dotfiles") — must be non-empty.
func parseGitPath(urlPath string) (repoName, endpoint string, ok bool) {
	p := strings.TrimPrefix(urlPath, "/")
	for _, ep := range []string{"info/refs", "git-upload-pack", "git-receive-pack"} {
		if suffix := "/" + ep; strings.HasSuffix(p, suffix) {
			name := strings.TrimSuffix(p, suffix)
			if name == "" {
				return "", "", false
			}
			return name, ep, true
		}
	}
	return "", "", false
}

// gitConfigEnv renders config overrides as the GIT_CONFIG_COUNT/KEY/VALUE
// environment git honors in every process, so they reach the spawned
// receive-pack (and its hooks).
func gitConfigEnv(pairs ...[2]string) []string {
	env := []string{fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(pairs))}
	for i, p := range pairs {
		env = append(env,
			fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, p[0]),
			fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, p[1]),
		)
	}
	return env
}
