// Package githttp serves the git smart-HTTP read path (clone / fetch). It
// materializes the target repo (refs from s3lite, objects hydrated from the
// object store) and then delegates all protocol work — ref advertisement and
// pack negotiation — to stock `git http-backend` over CGI, per the read path
// in docs/architecture/request-flows.md. Only the upload-pack (read) service is
// routed here; the write path (receive-pack) lands in task 07.
package githttp

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/cgi"
	"os/exec"
	"strings"

	"github.com/atmin/gitmote/internal/meta"
	"github.com/atmin/gitmote/internal/repo"
)

// Handler serves the read-path smart-HTTP endpoints for any repo, dispatching
// on the git URL suffix. Mount it at "/"; more specific routes (e.g. /healthz)
// still win under http.ServeMux.
type Handler struct {
	mz      *repo.Materializer
	gitPath string
	logger  *slog.Logger
}

// New returns a read-path handler backed by mz. It fails if the `git`
// executable is not on PATH, since the whole design delegates to it.
func New(mz *repo.Materializer, logger *slog.Logger) (*Handler, error) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("githttp: git not found on PATH: %w", err)
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Handler{mz: mz, gitPath: gitPath, logger: logger}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	repoName, endpoint, ok := parseReadPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	// Serve only the upload-pack (read) service. receive-pack — and a
	// receive-pack advertisement — are not part of the read path.
	switch endpoint {
	case "info/refs":
		if r.Method != http.MethodGet || r.URL.Query().Get("service") != "git-upload-pack" {
			http.NotFound(w, r)
			return
		}
	case "git-upload-pack":
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
	}

	if _, err := h.mz.Materialize(r.Context(), repoName); err != nil {
		if errors.Is(err, meta.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		h.logger.Error("materialize failed", "repo", repoName, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Delegate to git-http-backend. PATH_INFO carries the full request path
	// (empty CGI Root) and GIT_PROJECT_ROOT is the cache root, so http-backend
	// resolves the repo dir the materializer just wrote.
	backend := &cgi.Handler{
		Path: h.gitPath,
		Args: []string{"http-backend"},
		Dir:  h.mz.Root(),
		Env: []string{
			"GIT_PROJECT_ROOT=" + h.mz.Root(),
			"GIT_HTTP_EXPORT_ALL=1",
		},
	}
	backend.ServeHTTP(w, r)
}

// parseReadPath splits a git smart-HTTP read URL into the repo name and the
// endpoint suffix ("info/refs" or "git-upload-pack"). The repo name — which may
// contain slashes ("atmin/dotfiles") — must be non-empty.
func parseReadPath(urlPath string) (repoName, endpoint string, ok bool) {
	p := strings.TrimPrefix(urlPath, "/")
	for _, ep := range []string{"info/refs", "git-upload-pack"} {
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
