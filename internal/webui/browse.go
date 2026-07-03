package webui

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/atmin/gitmote/internal/meta"
	"github.com/atmin/gitmote/internal/repo"
)

// logLimit caps how many commits a single history page shows; a 101st commit,
// if present, becomes the "more" marker rather than being silently dropped.
const logLimit = 100

// browse dispatches a /browse/{repo}/-/{action}/... request. Repo names contain
// slashes and Go's trailing wildcard can't carry a suffix, so the path is split
// manually on the "/-/" marker (GitLab's convention) into the repo name and an
// action tail; ref travels as a query parameter so a slashed branch needs no
// disambiguation from the path.
func (h *Handler) browse(w http.ResponseWriter, r *http.Request) {
	repoName, tail, ok := strings.Cut(r.PathValue("rest"), "/-/")
	if !ok || repoName == "" {
		http.NotFound(w, r)
		return
	}
	action, arg, _ := strings.Cut(tail, "/")
	switch action {
	case "tree":
		h.browseTree(w, r, repoName, arg)
	case "blob":
		h.browseBlob(w, r, repoName, arg)
	case "raw":
		h.browseRaw(w, r, repoName, arg)
	case "commits":
		h.browseCommits(w, r, repoName)
	case "commit":
		h.browseCommit(w, r, repoName, arg)
	default:
		http.NotFound(w, r)
	}
}

// browseCtx is the resolved starting point every browse handler needs: the repo
// record, its materialized on-disk dir, the selected ref, and the commit SHA
// that ref points at.
type browseCtx struct {
	repo *meta.Repo
	dir  string
	ref  string
	sha  string
}

// resolve materializes the repo, picks the ref (query "ref", else the default
// branch), and resolves it to a commit. It writes a 404 for an unknown repo or
// ref (and a 500 for anything else) and returns ok=false in those cases.
func (h *Handler) resolve(w http.ResponseWriter, r *http.Request, repoName string) (browseCtx, bool) {
	ctx := r.Context()
	rp, err := h.md.GetRepo(ctx, repoName)
	if errors.Is(err, meta.ErrNotFound) {
		http.NotFound(w, r)
		return browseCtx{}, false
	}
	if err != nil {
		h.serverError(w, "get repo", err)
		return browseCtx{}, false
	}
	ref := strings.TrimSpace(r.URL.Query().Get("ref"))
	if ref == "" {
		ref = rp.DefaultBranch
	}
	dir, err := h.mz.Materialize(ctx, repoName)
	if err != nil {
		h.serverError(w, "materialize repo", err)
		return browseCtx{}, false
	}
	sha, err := repo.ResolveRef(ctx, dir, ref)
	if errors.Is(err, repo.ErrNotFound) {
		http.NotFound(w, r)
		return browseCtx{}, false
	}
	if err != nil {
		h.serverError(w, "resolve ref", err)
		return browseCtx{}, false
	}
	return browseCtx{repo: rp, dir: dir, ref: ref, sha: sha}, true
}

// base fills the shared browse header (nav identity, repo, selected ref, and the
// ref switcher's options) for c.
func (h *Handler) browseHeader(r *http.Request, c browseCtx) browseBase {
	return browseBase{
		base: h.base(r, "", ""),
		Repo: c.repo.Name,
		Ref:  c.ref,
		Refs: h.refChoices(r, c.repo.ID),
	}
}

// refChoices lists the repo's branches and tags (short names) for the switcher.
// A ref-listing failure is non-fatal: the page still renders, just without a
// switcher.
func (h *Handler) refChoices(r *http.Request, repoID int64) []refChoice {
	refs, err := h.md.ListRefs(r.Context(), repoID)
	if err != nil {
		h.log.Error("list refs for switcher", "error", err)
		return nil
	}
	choices := make([]refChoice, 0, len(refs))
	for _, ref := range refs {
		name := ref.Name
		switch {
		case strings.HasPrefix(name, "refs/heads/"):
			name = strings.TrimPrefix(name, "refs/heads/")
		case strings.HasPrefix(name, "refs/tags/"):
			name = strings.TrimPrefix(name, "refs/tags/")
		}
		choices = append(choices, refChoice{Name: name, Ref: name})
	}
	return choices
}

func (h *Handler) browseTree(w http.ResponseWriter, r *http.Request, repoName, treePath string) {
	c, ok := h.resolve(w, r, repoName)
	if !ok {
		return
	}
	entries, err := repo.Tree(r.Context(), c.dir, c.sha, treePath)
	if errors.Is(err, repo.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.serverError(w, "list tree", err)
		return
	}
	h.render(w, "browse_tree.html", treeData{
		browseBase: h.browseHeader(r, c),
		Path:       treePath,
		Crumbs:     crumbs(treePath),
		Entries:    entries,
	})
}

func (h *Handler) browseBlob(w http.ResponseWriter, r *http.Request, repoName, blobPath string) {
	c, ok := h.resolve(w, r, repoName)
	if !ok {
		return
	}
	content, size, binary, err := repo.Blob(r.Context(), c.dir, c.sha, blobPath)
	if errors.Is(err, repo.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.serverError(w, "read blob", err)
		return
	}
	data := blobData{
		browseBase: h.browseHeader(r, c),
		Path:       blobPath,
		Crumbs:     crumbs(blobPath),
		Binary:     binary,
		Size:       size,
	}
	if !binary {
		data.Text = string(content)
	}
	h.render(w, "browse_blob.html", data)
}

func (h *Handler) browseRaw(w http.ResponseWriter, r *http.Request, repoName, blobPath string) {
	c, ok := h.resolve(w, r, repoName)
	if !ok {
		return
	}
	// Confirm the blob exists (404 before any bytes are written), then stream
	// it — never buffering the whole object.
	if _, err := repo.BlobSize(r.Context(), c.dir, c.sha, blobPath); errors.Is(err, repo.ErrNotFound) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		h.serverError(w, "stat blob", err)
		return
	}
	rc, err := repo.BlobReader(r.Context(), c.dir, c.sha, blobPath)
	if err != nil {
		h.serverError(w, "open blob", err)
		return
	}
	defer func() { _ = rc.Close() }()

	if ct := mime.TypeByExtension(path.Ext(blobPath)); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("Content-Disposition", "attachment; filename=\""+path.Base(blobPath)+"\"")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if _, err := io.Copy(w, rc); err != nil {
		h.log.Error("stream blob", "repo", repoName, "path", blobPath, "error", err)
	}
}

func (h *Handler) browseCommits(w http.ResponseWriter, r *http.Request, repoName string) {
	c, ok := h.resolve(w, r, repoName)
	if !ok {
		return
	}
	scope := strings.TrimSpace(r.URL.Query().Get("path"))
	commits, more, err := repo.Log(r.Context(), c.dir, c.sha, scope, logLimit)
	if errors.Is(err, repo.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.serverError(w, "commit log", err)
		return
	}
	h.render(w, "browse_commits.html", commitsData{
		browseBase: h.browseHeader(r, c),
		Path:       scope,
		Crumbs:     crumbs(scope),
		Commits:    commits,
		More:       more,
	})
}

func (h *Handler) browseCommit(w http.ResponseWriter, r *http.Request, repoName, sha string) {
	c, ok := h.resolve(w, r, repoName)
	if !ok {
		return
	}
	commit, diff, err := repo.Show(r.Context(), c.dir, sha)
	if errors.Is(err, repo.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.serverError(w, "show commit", err)
		return
	}
	h.render(w, "browse_commit.html", commitData{
		browseBase: h.browseHeader(r, c),
		Commit:     commit,
		Diff:       diff,
	})
}

// crumbs turns a path into its breadcrumb trail, each segment linking to the
// tree at that depth. The empty path yields no crumbs (the repo root link in
// the template stands alone).
func crumbs(p string) []crumb {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	var out []crumb
	var acc string
	for _, seg := range strings.Split(p, "/") {
		if acc == "" {
			acc = seg
		} else {
			acc += "/" + seg
		}
		out = append(out, crumb{Name: seg, Path: acc})
	}
	return out
}
