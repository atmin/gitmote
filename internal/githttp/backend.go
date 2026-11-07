// Package githttp serves git smart-HTTP. It materializes the target repo:
// refs come from s3lite, and objects are hydrated from the object store. It
// delegates the protocol work to stock `git http-backend` over CGI:
//   - the ref advertisement
//   - the pack negotiation
//   - receive-pack
//
// The request flows are in docs/architecture/request-flows.md.
//
// The read path (clone or fetch, upload-pack) needs only a materialized repo.
// The write path (push, receive-pack) does three more things:
//   - It serializes pushes per repo.
//   - It mints a nonce for each push.
//   - It installs the pre-receive hook, which calls the parent over RPC to
//     enforce the content-before-pointer CAS.
//
// The Writer (receive.go) holds this machinery. The handler uses it only when
// it is built with a Writer.
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

// Authorizer guards a request before it is served. It authenticates the PAT
// and checks that the user holds perm on repoName. It returns one of these:
//   - an error sentinel of auth (ErrUnauthorized or ErrForbidden)
//   - meta.ErrNotFound
//   - an internal error
type Authorizer interface {
	Authorize(r *http.Request, repoName string, perm meta.Perm) (*meta.User, error)
}

// Config assembles a Handler.
type Config struct {
	// Materializer builds the on-disk repo git-http-backend serves. Required.
	Materializer *repo.Materializer
	// Authorizer guards every request. Required.
	Authorizer Authorizer
	// Writer enables the push path. Nil serves the read path only. The write
	// endpoints then return 404.
	Writer *Writer
	// IsWritable reports whether this instance may serve writes. That is, it
	// holds the litestream lease (see meta.Metadata.IsLeader). Nil means always
	// writable, as in an unreplicated setup or in tests. A follower refuses
	// receive-pack with a retryable 503. Reads are not affected.
	IsWritable func() bool
	// Logger is optional. Nil discards the logs.
	Logger *slog.Logger
}

// Handler serves the git smart-HTTP endpoints for any repo, dispatching on the
// git URL suffix. Mount it at "/". Under http.ServeMux, more specific routes
// (for example /healthz) still win.
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
	// Only the writer that holds the lease serves receive-pack. A read-only
	// follower refuses pushes (safety.md §1) with a retryable 503. The client
	// can then try again once the leader is up. This gates both the
	// advertisement and the push POST. Both roles serve reads (upload-pack).
	if isReceive && h.isWritable != nil && !h.isWritable() {
		w.Header().Set("Retry-After", "5")
		http.Error(w, "gitmote: not the current writer, retry shortly", http.StatusServiceUnavailable)
		return
	}

	user, err := h.authz.Authorize(r, repoName, perm)
	if err != nil {
		h.writeAuthError(w, r, repoName, err)
		return
	}

	if isPush {
		h.serveReceivePack(w, r, repoName, user.ID)
		return
	}

	// For a read or a receive-pack advertisement, materialize the repo and then
	// hand off to http-backend. The receive advertisement needs
	// http.receivepack enabled.
	//
	// The info/refs advertisement only lists refs, so it materializes refs
	// alone, with no object hydration (docs/architecture/storage.md).
	// The upload-pack POST that transfers history still hydrates fully.
	materialized := h.materialize
	if endpoint == "info/refs" {
		materialized = h.materializeRefs
	}
	if !materialized(w, r, repoName) {
		return
	}
	var extra []string
	if isReceive {
		extra = gitConfigEnv([2]string{"http.receivepack", "true"})
	}
	h.serveCGI(w, r, extra)
}

// serveReceivePack runs a push under the per-repo write lock. It does these
// steps:
//   - Refresh the repo from the sources of truth.
//   - Mint a nonce.
//   - Hand off to receive-pack, with the pre-receive hook set to call back on
//     the socket.
//
// The callback of the hook does the object PUT and the ref CAS (see
// Writer.handle).
func (h *Handler) serveReceivePack(w http.ResponseWriter, r *http.Request, repoName string, pusherID int64) {
	push, err := h.writer.Begin(r.Context(), repoName, pusherID)
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

	// Materialize under the lock, so that the fast-forward check of
	// receive-pack runs against the authoritative tip.
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

	// receive-pack has finished: the response is flushed, and the on-disk refs
	// are updated. Fire the after-commit hook at this point. A CI dispatch then
	// reads a warm, consistent repo. It never races the ref finalization of
	// receive-pack. The hook is fire-and-forget. It never fails the push, which
	// is already committed.
	h.writer.FireAfterCommit(pusherID, push.Committed())
}

// materialize builds the on-disk repo, writing the HTTP error itself on failure
// and returning false so the caller stops.
func (h *Handler) materialize(w http.ResponseWriter, r *http.Request, repoName string) bool {
	_, err := h.mz.Materialize(r.Context(), repoName)
	return h.finishMaterialize(w, r, repoName, err)
}

// materializeRefs makes the ref advertisement of the repo current without
// hydrating objects. The info/refs path uses it. It has the same signature and
// error handling as materialize.
func (h *Handler) materializeRefs(w http.ResponseWriter, r *http.Request, repoName string) bool {
	_, err := h.mz.MaterializeRefs(r.Context(), repoName)
	return h.finishMaterialize(w, r, repoName, err)
}

// finishMaterialize maps a materialize error to the HTTP response, writing a 404
// for an unknown repo and a 500 otherwise, and returns whether it succeeded.
func (h *Handler) finishMaterialize(w http.ResponseWriter, r *http.Request, repoName string, err error) bool {
	if err != nil {
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
// path, because the CGI Root is empty. GIT_PROJECT_ROOT is the cache root. So
// http-backend resolves the repo directory that the materializer just wrote.
func (h *Handler) serveCGI(w http.ResponseWriter, r *http.Request, extraEnv []string) {
	// git http-backend runs via CGI and needs CONTENT_LENGTH. A request with an
	// unknown length carries none, and http-backend rejects it with 400. Git
	// sends such a request (chunked transfer-encoding) for a pack larger than
	// the http.postBuffer of the client (default 1 MiB). Buffer such a body to a
	// temp file to give it a fixed length before the hand-off. A large push
	// then works with no client http.postBuffer tuning. Bodies with a known
	// length stream through unchanged.
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

// bufferBodyToFile drains r.Body to a temp file. It then sets the request to
// read from that file with a known ContentLength. This turns a body of unknown
// length (chunked) into a body of fixed length for the CGI child. It returns a
// cleanup function that closes and removes the file after the response is
// served. The body goes to disk, not to memory, so a large pack stays off the
// heap.
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

// writeAuthError maps an Authorize error to the HTTP response that git
// expects:
//   - A 401 carries the Basic challenge, so git and its credential helpers
//     retry with credentials.
//   - A 403 is a hard stop.
//   - A missing repo is a 404.
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

// classify takes a parsed endpoint and a request, and determines these:
//   - perm: the required permission
//   - isReceive: whether the request targets receive-pack
//   - isPush: whether the request mutates the repo
//
// ok=false rejects a wrong method or an unknown service.
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
// must be non-empty. The parser accepts slashes, but meta rejects a repo name
// with a slash, so no such repo exists.
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

// gitConfigEnv renders config overrides as git environment variables:
//   - GIT_CONFIG_COUNT, the number of pairs.
//   - GIT_CONFIG_KEY_<n>, the key of pair n.
//   - GIT_CONFIG_VALUE_<n>, the value of pair n.
//
// Git honors them in every process, so they reach the spawned receive-pack
// and its hooks.
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
