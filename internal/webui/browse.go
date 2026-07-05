package webui

import (
	"errors"
	"html/template"
	"io"
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/atmin/gitmote/internal/meta"
	"github.com/atmin/gitmote/internal/render"
	"github.com/atmin/gitmote/internal/repo"
)

// logLimit caps how many commits a single history page shows; a 101st commit,
// if present, becomes the "more" marker rather than being silently dropped.
const logLimit = 100

// browseLanding serves the bare /{repo}: the README + tree at the default
// branch (an empty ref selects it, an empty path is the repo root).
func (h *Handler) browseLanding(w http.ResponseWriter, r *http.Request) {
	repoName := r.PathValue("repo")
	if !h.authorizeRead(w, r, repoName) {
		return
	}
	c, _, ok := h.resolve(w, r, repoName, "")
	if !ok {
		return
	}
	h.renderTree(w, r, c, "")
}

// browseTreeRoute serves /{repo}/tree/<ref>/<path>: the unified content verb. It
// renders a directory listing for a tree and the file view for a blob, chosen by
// the entry's type.
func (h *Handler) browseTreeRoute(w http.ResponseWriter, r *http.Request) {
	repoName := r.PathValue("repo")
	if !h.authorizeRead(w, r, repoName) {
		return
	}
	c, p, ok := h.resolve(w, r, repoName, r.PathValue("rest"))
	if !ok {
		return
	}
	typ, err := repo.EntryType(r.Context(), c.dir, c.sha, p)
	if errors.Is(err, repo.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.serverError(w, "entry type", err)
		return
	}
	if typ == "blob" {
		h.renderBlob(w, r, c, p)
		return
	}
	h.renderTree(w, r, c, p)
}

// browseBlobRoute serves /{repo}/blob/<ref>/<path>: an explicit file view. A
// path that is actually a directory 301s to its tree URL (canonicalize, don't
// 404), so blob self-heals and tree never guesses wrong.
func (h *Handler) browseBlobRoute(w http.ResponseWriter, r *http.Request) {
	repoName := r.PathValue("repo")
	if !h.authorizeRead(w, r, repoName) {
		return
	}
	c, p, ok := h.resolve(w, r, repoName, r.PathValue("rest"))
	if !ok {
		return
	}
	typ, err := repo.EntryType(r.Context(), c.dir, c.sha, p)
	if errors.Is(err, repo.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.serverError(w, "entry type", err)
		return
	}
	if typ == "tree" {
		http.Redirect(w, r, treeURL(c.repo.Name, c.ref, p), http.StatusMovedPermanently)
		return
	}
	h.renderBlob(w, r, c, p)
}

// browseRawRoute serves /{repo}/raw/<ref>/<path>: the file's bytes. It is
// file-only — a directory 404s (BlobSize fails on a non-blob).
func (h *Handler) browseRawRoute(w http.ResponseWriter, r *http.Request) {
	repoName := r.PathValue("repo")
	if !h.authorizeRead(w, r, repoName) {
		return
	}
	c, p, ok := h.resolve(w, r, repoName, r.PathValue("rest"))
	if !ok {
		return
	}
	h.renderRaw(w, r, c, p)
}

// browseCommitsRoute serves /{repo}/commits/<ref>/<path>: the history of ref,
// optionally scoped to a path.
func (h *Handler) browseCommitsRoute(w http.ResponseWriter, r *http.Request) {
	repoName := r.PathValue("repo")
	if !h.authorizeRead(w, r, repoName) {
		return
	}
	c, p, ok := h.resolve(w, r, repoName, r.PathValue("rest"))
	if !ok {
		return
	}
	h.renderCommits(w, r, c, p)
}

// browseCommitRoute serves /{repo}/commit/<sha>: one commit's metadata and diff.
// The URL carries no ref, so the header context resolves the default branch.
func (h *Handler) browseCommitRoute(w http.ResponseWriter, r *http.Request) {
	repoName := r.PathValue("repo")
	if !h.authorizeRead(w, r, repoName) {
		return
	}
	c, _, ok := h.resolve(w, r, repoName, "")
	if !ok {
		return
	}
	h.renderCommit(w, r, c, r.PathValue("sha"))
}

// browseRefsRoute serves /{repo}/refs: the repo's branches and tags.
func (h *Handler) browseRefsRoute(w http.ResponseWriter, r *http.Request) {
	repoName := r.PathValue("repo")
	if !h.authorizeRead(w, r, repoName) {
		return
	}
	c, _, ok := h.resolve(w, r, repoName, "")
	if !ok {
		return
	}
	h.render(w, "browse_refs.html", refsData{
		browseBase: h.browseHeader(r, c),
		Refs:       h.refChoices(r, c.repo.ID),
	})
}

// ciRunsRoute serves /{repo}/runs (the list) and /{repo}/runs/<id>[/job/…] (one
// run or a job log), sharing the repo-read gate with the other browse verbs.
func (h *Handler) ciRunsRoute(w http.ResponseWriter, r *http.Request) {
	repoName := r.PathValue("repo")
	if !h.authorizeRead(w, r, repoName) {
		return
	}
	if rest := r.PathValue("rest"); rest != "" {
		h.ciRun(w, r, repoName, rest)
		return
	}
	h.ciRuns(w, r, repoName)
}

// treeURL builds the canonical tree URL for a ref+path (ref and path may both
// contain slashes; both live in the path, never a query).
func treeURL(repoName, ref, p string) string {
	u := "/" + repoName + "/tree/" + ref
	if p != "" {
		u += "/" + p
	}
	return u
}

// authorizeRead gates a browse request on repo-read: a public repo is readable
// by anyone, a private repo needs a signed-in user with an ACL (CanRead). It
// writes the response and returns false on refusal — 404 for an unknown repo, a
// login redirect for an anonymous viewer of a private repo, 403 for a signed-in
// viewer without access. Management routes stay admin-gated (requireAdmin).
func (h *Handler) authorizeRead(w http.ResponseWriter, r *http.Request, repoName string) bool {
	// The viewer is optional here: a valid session resolves to its user, else the
	// request is anonymous (uid 0), which CanRead allows only for a public repo.
	var uid int64
	if id, ok := h.sess.verify(r, h.now()); ok {
		if u, err := h.md.GetUserByID(r.Context(), id); err == nil {
			uid = u.ID
		}
	}
	rp, err := h.md.GetRepo(r.Context(), repoName)
	if errors.Is(err, meta.ErrNotFound) {
		// Don't reveal a private forge's inventory to anonymous callers: a missing
		// repo redirects to login rather than 404ing (a signed-in user gets the 404).
		if uid == 0 {
			h.denyUnauth(w, r)
			return false
		}
		http.NotFound(w, r)
		return false
	}
	if err != nil {
		h.serverError(w, "get repo", err)
		return false
	}
	can, err := h.md.CanRead(r.Context(), rp, uid)
	if err != nil {
		h.serverError(w, "authorize read", err)
		return false
	}
	if can {
		return true
	}
	if uid == 0 {
		h.denyUnauth(w, r) // anonymous → login
		return false
	}
	http.Error(w, "forbidden", http.StatusForbidden)
	return false
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

// resolve turns a repo name and an in-path "rest" (ref + path, both possibly
// slashed) into a browseCtx and the remaining path. The ref is resolved greedily
// against the repo's refs — the longest leading prefix of rest that names a real
// branch or tag (branch beats a tag on a tie); an empty rest selects the default
// branch. It writes a 404 for an unknown repo, a rest that names no ref, or a
// ref that fails to resolve (and a 500 for anything else), returning ok=false.
func (h *Handler) resolve(w http.ResponseWriter, r *http.Request, repoName, rest string) (browseCtx, string, bool) {
	ctx := r.Context()
	rp, err := h.md.GetRepo(ctx, repoName)
	if errors.Is(err, meta.ErrNotFound) {
		http.NotFound(w, r)
		return browseCtx{}, "", false
	}
	if err != nil {
		h.serverError(w, "get repo", err)
		return browseCtx{}, "", false
	}
	refs, err := h.md.ListRefs(ctx, rp.ID)
	if err != nil {
		h.serverError(w, "list refs", err)
		return browseCtx{}, "", false
	}
	ref, treePath, branch, ok := greedyRef(refs, rest, rp.DefaultBranch)
	if !ok {
		http.NotFound(w, r) // rest names no ref
		return browseCtx{}, "", false
	}
	dir, err := h.mz.Materialize(ctx, repoName)
	if err != nil {
		h.serverError(w, "materialize repo", err)
		return browseCtx{}, "", false
	}
	// Resolve the fully-qualified ref so a branch beats a same-named tag (git's
	// bare-name precedence favors tags) and an annotated tag peels to its commit.
	sha, err := repo.ResolveRef(ctx, dir, qualify(ref, branch))
	if errors.Is(err, repo.ErrNotFound) {
		http.NotFound(w, r)
		return browseCtx{}, "", false
	}
	if err != nil {
		h.serverError(w, "resolve ref", err)
		return browseCtx{}, "", false
	}
	return browseCtx{repo: rp, dir: dir, ref: ref, sha: sha}, treePath, true
}

// greedyRef splits rest into a ref name and the remaining path: the longest
// leading prefix of rest that names a real branch or tag. A branch beats a tag
// on the same prefix; a longer prefix beats a shorter one regardless of type. An
// empty rest selects defaultBranch with an empty path. branch reports whether
// the chosen ref is a branch (to qualify it for resolution); ok is false when no
// prefix names a ref.
func greedyRef(refs []meta.Ref, rest, defaultBranch string) (name, treePath string, branch, ok bool) {
	branches := make(map[string]bool)
	tags := make(map[string]bool)
	for _, rf := range refs {
		if n, found := strings.CutPrefix(rf.Name, "refs/heads/"); found {
			branches[n] = true
		} else if n, found := strings.CutPrefix(rf.Name, "refs/tags/"); found {
			tags[n] = true
		}
	}
	rest = strings.Trim(rest, "/")
	if rest == "" {
		return defaultBranch, "", true, true
	}
	segs := strings.Split(rest, "/")
	for i := len(segs); i > 0; i-- {
		cand := strings.Join(segs[:i], "/")
		tail := strings.Join(segs[i:], "/")
		if branches[cand] {
			return cand, tail, true, true
		}
		if tags[cand] {
			return cand, tail, false, true
		}
	}
	return "", "", false, false
}

// qualify makes a ref name unambiguous for resolution: refs/heads/<name> for a
// branch, refs/tags/<name> for a tag.
func qualify(name string, branch bool) string {
	if branch {
		return "refs/heads/" + name
	}
	return "refs/tags/" + name
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

func (h *Handler) renderTree(w http.ResponseWriter, r *http.Request, c browseCtx, treePath string) {
	entries, err := repo.Tree(r.Context(), c.dir, c.sha, treePath)
	if errors.Is(err, repo.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.serverError(w, "list tree", err)
		return
	}
	readme := h.readme(r, c, entries)
	data := treeData{
		browseBase: h.browseHeader(r, c),
		Path:       treePath,
		Crumbs:     crumbs(treePath),
		Entries:    entries,
		Readme:     readme,
	}
	data.Mermaid = render.HasMermaid(readme)
	h.render(w, "browse_tree.html", data)
}

func (h *Handler) renderBlob(w http.ResponseWriter, r *http.Request, c browseCtx, blobPath string) {
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
	// Text under the size cap is rendered: markdown for a .md blob, otherwise
	// syntax-highlighted source. Anything else (binary, oversize, or a
	// highlight error) falls back to the plain <pre> on .Text.
	switch {
	case binary:
	case size > render.MaxSize:
		data.Text = string(content)
	case render.IsMarkdown(blobPath):
		data.Rendered = render.Markdown(content)
		data.Mermaid = render.HasMermaid(data.Rendered)
	default:
		if hl, err := render.Highlight(content, blobPath); err == nil {
			data.Highlighted = hl
		} else {
			data.Text = string(content)
		}
	}
	h.render(w, "browse_blob.html", data)
}

func (h *Handler) renderRaw(w http.ResponseWriter, r *http.Request, c browseCtx, blobPath string) {
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
		h.log.Error("stream blob", "repo", c.repo.Name, "path", blobPath, "error", err)
	}
}

func (h *Handler) renderCommits(w http.ResponseWriter, r *http.Request, c browseCtx, scope string) {
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

func (h *Handler) renderCommit(w http.ResponseWriter, r *http.Request, c browseCtx, sha string) {
	commit, diff, err := repo.Show(r.Context(), c.dir, sha)
	if errors.Is(err, repo.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.serverError(w, "show commit", err)
		return
	}
	// Best-effort CI badge: the latest run for this exact commit, if any. A
	// lookup failure is non-fatal — the commit still renders, just without a badge.
	var run *meta.Run
	if got, err := h.md.LatestRunForSHA(r.Context(), c.repo.ID, commit.SHA); err == nil {
		run = got
	} else if !errors.Is(err, meta.ErrNotFound) {
		h.log.Error("latest run for sha", "repo", c.repo.Name, "sha", commit.SHA, "error", err)
	}
	h.render(w, "browse_commit.html", commitData{
		browseBase: h.browseHeader(r, c),
		Commit:     commit,
		Diff:       diff,
		Run:        run,
	})
}

// readme renders a directory's README.md (if any) to HTML for display below
// the tree listing. It is best-effort: a missing, binary, oversize, or
// unreadable README simply yields no rendered block.
func (h *Handler) readme(r *http.Request, c browseCtx, entries []repo.TreeEntry) template.HTML {
	for _, e := range entries {
		if e.Type != "blob" || !render.IsReadme(e.Name) {
			continue
		}
		content, size, binary, err := repo.Blob(r.Context(), c.dir, c.sha, e.Path)
		if err != nil || binary || size > render.MaxSize {
			return ""
		}
		return render.Markdown(content)
	}
	return ""
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
