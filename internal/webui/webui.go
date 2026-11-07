// Package webui serves the web UI (the "web UI" component in
// docs/architecture/storage.md). It is a thin server-rendered layer over the
// same s3lite tables and object store that the git path uses. It introduces no
// new source of truth.
//
// The UI covers the operations that git can't do. Management is gated on the
// global-admin role (users.is_admin). Management covers these operations:
//   - creating repos
//   - minting and revoking tokens
//   - managing users
//   - managing per-repo settings, ACLs and secrets
//
// The other pages are scoped to the viewer and need no admin role. The
// dashboard lists the repos that the viewer can read. Read-only browsing
// serves the materialized bare repo as tree, blob, raw, commits and commit
// pages. Browsing is gated on repo-read. Anyone can read a public repo, and a
// private repo needs an ACL (see authorizeRead).
//
// A browser logs in by pasting a personal access token once. The server
// verifies the token through the same path that git auth uses. The server then
// issues a signed, stateless session cookie.
package webui

import (
	"context"
	"embed"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/atmin/gitmote/internal/auth"
	"github.com/atmin/gitmote/internal/meta"
	"github.com/atmin/gitmote/internal/render"
	"github.com/atmin/gitmote/internal/repo"
	"github.com/atmin/gitmote/internal/store"
)

//go:embed templates/*.html
var templatesFS embed.FS

// mermaidJS is the vendored, version-pinned mermaid library. It is embedded and
// served from the same origin, so diagram rendering needs no external fetch
// (see static/README.md).
//
//go:embed static/mermaid.min.js
var mermaidJS []byte

// Authenticator verifies a raw personal access token and resolves it to its
// owner. *auth.Guard satisfies it. The login form is the only caller.
type Authenticator interface {
	VerifyToken(ctx context.Context, raw string) (*meta.User, error)
}

// SecretsAdmin manages a repo's CI secrets for the admin panel. Values are
// write-only: only names are ever read back. *secrets.Service satisfies it. A
// nil SecretsAdmin disables the secrets UI.
type SecretsAdmin interface {
	Enabled() bool
	SetSecret(ctx context.Context, repoID int64, name, value string) error
	ListSecretNames(ctx context.Context, repoID int64) ([]string, error)
	DeleteSecret(ctx context.Context, repoID int64, name string) error
}

// nameSegment matches a valid owner or repo path segment.
var nameSegment = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// LiveLog reads the live tail of a job's CI log from memory. The tail is the
// leader's ephemeral buffer, which the browser polls while a job runs.
// *ci.LiveLogs satisfies it. A nil LiveLog disables the live tail. The UI then
// shows only the durable log, once the job completes.
type LiveLog interface {
	Read(jobID int64, offset int) (data []byte, next int, done, ok bool)
}

// Handler serves the management UI.
type Handler struct {
	md      *meta.Metadata
	mz      *repo.Materializer
	store   store.Store
	live    LiveLog
	auth    Authenticator
	secrets SecretsAdmin
	sess    *sessions
	tmpl    *template.Template
	log     *slog.Logger
	now     func() time.Time
}

// New builds the UI handler. The arguments do the following:
//   - cookieKey signs session cookies and must be non-empty.
//   - The auth verifier (a) backs the login form.
//   - mz materializes repos for the read-only browse pages.
//   - objs reads CI log blobs for the run views.
//   - live backs the live log tail of a running job. It may be nil.
//   - secrets backs the CI secrets panel. It may be nil.
func New(md *meta.Metadata, mz *repo.Materializer, objs store.Store, live LiveLog, a Authenticator, secrets SecretsAdmin, cookieKey []byte, logger *slog.Logger) (*Handler, error) {
	if len(cookieKey) == 0 {
		return nil, errors.New("webui: empty cookie key")
	}
	tmpl, err := template.New("webui").Funcs(template.FuncMap{
		// parentPath is the enclosing directory of a browse path, or "" at the
		// root. Only the ".." link in the tree view uses it.
		"parentPath": func(p string) string {
			p = strings.Trim(p, "/")
			if i := strings.LastIndex(p, "/"); i >= 0 {
				return p[:i]
			}
			return ""
		},
		// highlightCSS is chroma's class stylesheet, included once in the head so
		// highlighted blobs and README code blocks share one theme.
		"highlightCSS": render.HighlightCSS,
		// short abbreviates a commit SHA for display.
		"short": func(s string) string {
			if len(s) > 8 {
				return s[:8]
			}
			return s
		},
		// statusColor maps a CI run or job status to a CSS color for its badge.
		"statusColor": func(s meta.RunStatus) string {
			switch s {
			case meta.RunPassed:
				return "#27ae60"
			case meta.RunFailed, meta.RunError:
				return "#c0392b"
			case meta.RunRunning:
				return "#b7950b"
			default: // queued, superseded
				return "#7f8c8d"
			}
		},
	}).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{
		md:      md,
		mz:      mz,
		store:   objs,
		live:    live,
		auth:    a,
		secrets: secrets,
		sess:    &sessions{key: cookieKey},
		tmpl:    tmpl,
		log:     logger,
		now:     time.Now,
	}, nil
}

// Register mounts the UI routes on mux. The dashboard, login and read-only
// browsing are reachable without authentication, and are scoped to the viewer.
// The global-management and per-repo-management routes require a global-admin
// session.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /login", h.getLogin)
	mux.HandleFunc("POST /login", h.postLogin)
	mux.HandleFunc("POST /logout", h.postLogout)

	// The bare root is the dashboard: a repo list scoped to the viewer, not a
	// redirect. GET /{$} matches only the exact root, so it never shadows a repo
	// path. POST /{$} is the admin-only repo-create. Both routes exist only when
	// the UI exists.
	mux.HandleFunc("GET /{$}", h.getDashboard)
	mux.HandleFunc("POST /{$}", h.requireAdmin(h.postRepo))

	// Global management is top-level and admin-only, with no /ui/ prefix.
	mux.HandleFunc("GET /users", h.requireAdmin(h.getUsers))
	mux.HandleFunc("POST /users", h.requireAdmin(h.postUser))
	mux.HandleFunc("GET /tokens", h.requireAdmin(h.getTokens))
	mux.HandleFunc("POST /tokens", h.requireAdmin(h.postToken))
	mux.HandleFunc("POST /tokens/revoke", h.requireAdmin(h.postRevokeToken))

	// Per-repo management uses verbs in the second path segment and is
	// admin-only. The routes are enumerated, never a broad /{repo}/{rest...}, so
	// git's own suffixes still fall through to the catch-all.
	mux.HandleFunc("GET /{repo}/settings", h.requireAdmin(h.getSettings))
	mux.HandleFunc("POST /{repo}/settings", h.requireAdmin(h.postSettings))
	mux.HandleFunc("GET /{repo}/access", h.requireAdmin(h.getAccess))
	mux.HandleFunc("POST /{repo}/access", h.requireAdmin(h.postAccess))
	mux.HandleFunc("POST /{repo}/access/revoke", h.requireAdmin(h.postRevokeAccess))
	mux.HandleFunc("GET /{repo}/secrets", h.requireAdmin(h.getSecrets))
	mux.HandleFunc("POST /{repo}/secrets", h.requireAdmin(h.postSecret))
	mux.HandleFunc("POST /{repo}/secrets/delete", h.requireAdmin(h.postDeleteSecret))

	// Read-only browsing is gated on repo-read, not on admin. Anyone can read a
	// public repo, and a private repo needs an ACL. This makes spectators and
	// public repos usable. Each handler does the per-request check through
	// authorizeRead.
	//
	// The repo is a single path segment. The ref lives in the path and is
	// resolved greedily, so each content verb has its own enumerated route.
	// Only these fixed verbs are registered, never a broad /{repo}/{rest...}.
	// Git's own paths, such as /{repo}/info/refs and /{repo}/git-upload-pack,
	// therefore fall through to the smart-HTTP catch-all at "/"
	// (docs/architecture/urls.md, "Implementation seams").
	//
	// Each verb registers a bare form and a "/{rest...}" form. A "/{rest...}"
	// pattern is a subtree, so ServeMux would otherwise 307-redirect the bare
	// "/{repo}/runs" to "/{repo}/runs/". The bare form (empty rest) selects the
	// default branch, or the run list for runs.
	mux.HandleFunc("GET /{repo}/tree", h.browseTreeRoute)
	mux.HandleFunc("GET /{repo}/tree/{rest...}", h.browseTreeRoute)
	mux.HandleFunc("GET /{repo}/blob", h.browseBlobRoute)
	mux.HandleFunc("GET /{repo}/blob/{rest...}", h.browseBlobRoute)
	mux.HandleFunc("GET /{repo}/raw", h.browseRawRoute)
	mux.HandleFunc("GET /{repo}/raw/{rest...}", h.browseRawRoute)
	mux.HandleFunc("GET /{repo}/commits", h.browseCommitsRoute)
	mux.HandleFunc("GET /{repo}/commits/{rest...}", h.browseCommitsRoute)
	mux.HandleFunc("GET /{repo}/commit/{sha}", h.browseCommitRoute)
	mux.HandleFunc("GET /{repo}/compare/{rest...}", h.browseCompareRoute)
	mux.HandleFunc("GET /{repo}/refs", h.browseRefsRoute)
	mux.HandleFunc("GET /{repo}/runs", h.ciRunsRoute)
	mux.HandleFunc("GET /{repo}/runs/{rest...}", h.ciRunsRoute)
	// The bare repo landing shows the README and the tree at the default branch.
	// It has one segment, so it is distinct from git's GET /{repo}/info/refs.
	mux.HandleFunc("GET /{repo}", h.browseLanding)

	// The vendored mermaid library for diagram rendering is served from the same
	// origin. It is public and non-sensitive (a pinned copy of an open-source
	// library), so it needs no admin session. It is hard-cached, because the
	// file itself is the version pin.
	mux.HandleFunc("GET /static/mermaid.min.js", h.serveMermaidJS)
}

// serveMermaidJS serves the embedded mermaid library for diagram rendering in
// the browse UI. The library is a trusted, fixed asset. The response is cached
// as immutable, so a browser fetches it once.
func (h *Handler) serveMermaidJS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "max-age=31536000, immutable")
	_, _ = w.Write(mermaidJS)
}

// ctxKey is the type of the context key (userKey) that carries the authenticated
// admin from the middleware to the handler.
type ctxKey int

const userKey ctxKey = 0

func userFrom(ctx context.Context) *meta.User {
	u, _ := ctx.Value(userKey).(*meta.User)
	return u
}

// requireAdmin resolves the session cookie to a current user on every request.
// It then checks that the user is still an admin. A missing or invalid session
// gives a redirect to login for GET, and 401 otherwise. An authenticated user
// who is not an admin gets 403.
func (h *Handler) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, ok := h.sess.verify(r, h.now())
		if !ok {
			h.denyUnauth(w, r)
			return
		}
		user, err := h.md.GetUserByID(r.Context(), uid)
		if errors.Is(err, meta.ErrNotFound) {
			h.denyUnauth(w, r)
			return
		}
		if err != nil {
			h.serverError(w, "load session user", err)
			return
		}
		if !user.IsAdmin {
			http.Error(w, "forbidden: admin only", http.StatusForbidden)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userKey, user)))
	}
}

func (h *Handler) denyUnauth(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

// --- login and logout ---

func (h *Handler) getLogin(w http.ResponseWriter, r *http.Request) {
	h.render(w, "login.html", loginData{})
}

func (h *Handler) postLogin(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSpace(r.FormValue("token"))
	user, err := h.auth.VerifyToken(r.Context(), token)
	if err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		h.render(w, "login.html", loginData{Err: "invalid token"})
		return
	}
	// Any valid user may sign in. A spectator or collaborator needs a session to
	// browse, on the dashboard, the private repos that they have access to.
	// Management stays admin-only. requireAdmin enforces it per route, not here.
	h.sess.issue(w, r, user.ID, h.now())
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *Handler) postLogout(w http.ResponseWriter, r *http.Request) {
	h.sess.clear(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// --- dashboard and repos ---

// getDashboard renders the repo list at "/", scoped to the viewer. Anyone sees
// the public repos. A viewer also sees each private repo on which they hold an
// ACL. An admin sees all repos. It is the unauthenticated landing: a signed-out
// visitor sees public repos and a sign-in link, never a 403 wall.
func (h *Handler) getDashboard(w http.ResponseWriter, r *http.Request) {
	h.renderDashboard(w, r, "", "")
}

func (h *Handler) renderDashboard(w http.ResponseWriter, r *http.Request, flash, errMsg string) {
	u := h.viewer(r)
	var (
		repos []meta.Repo
		err   error
	)
	if u != nil && u.IsAdmin {
		repos, err = h.md.ListRepos(r.Context())
	} else {
		repos, err = h.md.ListReposForViewer(r.Context(), userID(u))
	}
	if err != nil {
		h.serverError(w, "list repos", err)
		return
	}
	h.render(w, "dashboard.html", dashboardData{
		base:  h.baseFor(u, flash, errMsg),
		Repos: repos,
	})
}

func (h *Handler) postRepo(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	branch := strings.TrimSpace(r.FormValue("default_branch"))

	if !nameSegment.MatchString(name) {
		h.renderDashboard(w, r, "", "name must be alphanumeric (._- allowed, not leading)")
		return
	}
	// The namespace is flat: the repo is a single path segment. CreateRepo
	// enforces the reserved-name rules and the structural rules. The code below
	// shows its error to the user.
	repo, err := h.md.CreateRepo(r.Context(), name, branch)
	if err != nil {
		h.renderDashboard(w, r, "", "create repo: "+err.Error())
		return
	}
	// Grant the creating admin the admin permission on the new repo. The repo is
	// then usable at once (clone and push) without a separate ACL step. There is
	// no separate "owner". On a personal instance, the admin implicitly owns the
	// repos, and ACLs grant access to everyone else (docs/architecture/urls.md).
	// requireAdmin guarantees a user in the context.
	creator := userFrom(r.Context())
	if err := h.md.SetACL(r.Context(), repo.ID, creator.ID, meta.PermAdmin); err != nil {
		h.serverError(w, "grant creator acl", err)
		return
	}
	h.renderDashboard(w, r, "created "+name, "")
}

// --- per-repo settings ---

func (h *Handler) getSettings(w http.ResponseWriter, r *http.Request) {
	h.renderSettings(w, r, r.PathValue("repo"), "", "")
}

func (h *Handler) renderSettings(w http.ResponseWriter, r *http.Request, name, flash, errMsg string) {
	rp, err := h.md.GetRepo(r.Context(), name)
	if errors.Is(err, meta.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.serverError(w, "get repo", err)
		return
	}
	h.render(w, "settings.html", settingsData{
		base: h.base(r, flash, errMsg),
		Repo: *rp,
	})
}

// postSettings applies one setting per form. A form with a visibility value
// sets only the visibility. Otherwise, the form must carry a non-empty
// default_branch, or the handler shows an error.
func (h *Handler) postSettings(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("repo")
	rp, err := h.md.GetRepo(r.Context(), name)
	if errors.Is(err, meta.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.serverError(w, "get repo", err)
		return
	}
	if v := strings.TrimSpace(r.FormValue("visibility")); v != "" {
		if v != meta.VisibilityPrivate && v != meta.VisibilityPublic {
			h.renderSettings(w, r, name, "", "visibility must be private or public")
			return
		}
		if err := h.md.SetVisibility(r.Context(), rp.ID, v); err != nil {
			h.serverError(w, "set visibility", err)
			return
		}
		h.renderSettings(w, r, name, "visibility set to "+v, "")
		return
	}
	branch := strings.TrimSpace(r.FormValue("default_branch"))
	if branch == "" {
		h.renderSettings(w, r, name, "", "default branch cannot be empty")
		return
	}
	if err := h.md.SetDefaultBranch(r.Context(), rp.ID, branch); err != nil {
		h.serverError(w, "set default branch", err)
		return
	}
	h.renderSettings(w, r, name, "default branch set to "+branch, "")
}

// --- users ---

func (h *Handler) getUsers(w http.ResponseWriter, r *http.Request) {
	h.renderUsers(w, r, "", "")
}

func (h *Handler) renderUsers(w http.ResponseWriter, r *http.Request, flash, errMsg string) {
	users, err := h.md.ListUsers(r.Context())
	if err != nil {
		h.serverError(w, "list users", err)
		return
	}
	h.render(w, "users.html", usersData{
		base:  h.base(r, flash, errMsg),
		Users: users,
	})
}

func (h *Handler) postUser(w http.ResponseWriter, r *http.Request) {
	handle := strings.TrimSpace(r.FormValue("handle"))
	if !nameSegment.MatchString(handle) {
		h.renderUsers(w, r, "", "handle must be alphanumeric (._- allowed, not leading)")
		return
	}
	admin := r.FormValue("is_admin") != ""
	var err error
	if admin {
		_, err = h.md.CreateAdmin(r.Context(), handle)
	} else {
		_, err = h.md.CreateUser(r.Context(), handle)
	}
	if err != nil {
		h.renderUsers(w, r, "", "create user: "+err.Error())
		return
	}
	h.renderUsers(w, r, "created user "+handle, "")
}

// --- tokens ---

func (h *Handler) getTokens(w http.ResponseWriter, r *http.Request) {
	h.renderTokens(w, r, "", "", "")
}

// renderTokens lists the tokens of the selected user (the query or form value
// "user"). newRaw, when set, is the newly minted token. The page shows it
// exactly once.
func (h *Handler) renderTokens(w http.ResponseWriter, r *http.Request, selected, newRaw, errMsg string) {
	if selected == "" {
		selected = strings.TrimSpace(r.FormValue("user"))
	}
	users, err := h.md.ListUsers(r.Context())
	if err != nil {
		h.serverError(w, "list users", err)
		return
	}
	data := tokensData{
		base:     h.base(r, "", errMsg),
		Users:    users,
		Selected: selected,
		NewToken: newRaw,
	}
	if selected != "" {
		u, err := h.md.GetUser(r.Context(), selected)
		if errors.Is(err, meta.ErrNotFound) {
			data.Err = "no such user: " + selected
			h.render(w, "tokens.html", data)
			return
		}
		if err != nil {
			h.serverError(w, "get user", err)
			return
		}
		toks, err := h.md.ListTokens(r.Context(), u.ID)
		if err != nil {
			h.serverError(w, "list tokens", err)
			return
		}
		data.Tokens = toks
	}
	h.render(w, "tokens.html", data)
}

func (h *Handler) postToken(w http.ResponseWriter, r *http.Request) {
	handle := strings.TrimSpace(r.FormValue("user"))
	label := strings.TrimSpace(r.FormValue("label"))
	u, err := h.md.GetUser(r.Context(), handle)
	if errors.Is(err, meta.ErrNotFound) {
		h.renderTokens(w, r, handle, "", "no such user: "+handle)
		return
	}
	if err != nil {
		h.serverError(w, "get user", err)
		return
	}
	raw, selector, verifier, err := auth.Mint()
	if err != nil {
		h.serverError(w, "mint token", err)
		return
	}
	if _, err := h.md.CreateToken(r.Context(), u.ID, selector, verifier, label); err != nil {
		h.serverError(w, "store token", err)
		return
	}
	h.renderTokens(w, r, handle, raw, "")
}

func (h *Handler) postRevokeToken(w http.ResponseWriter, r *http.Request) {
	handle := strings.TrimSpace(r.FormValue("user"))
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		h.renderTokens(w, r, handle, "", "bad token id")
		return
	}
	if err := h.md.DeleteToken(r.Context(), id); err != nil {
		h.serverError(w, "revoke token", err)
		return
	}
	h.renderTokens(w, r, handle, "", "")
}

// --- per-repo access (ACLs) ---

func (h *Handler) getAccess(w http.ResponseWriter, r *http.Request) {
	h.renderAccess(w, r, r.PathValue("repo"), "", "")
}

func (h *Handler) renderAccess(w http.ResponseWriter, r *http.Request, name, flash, errMsg string) {
	rp, err := h.md.GetRepo(r.Context(), name)
	if errors.Is(err, meta.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.serverError(w, "get repo", err)
		return
	}
	acls, err := h.md.ListACLs(r.Context(), rp.ID)
	if err != nil {
		h.serverError(w, "list acls", err)
		return
	}
	h.render(w, "access.html", accessData{
		base: h.base(r, flash, errMsg),
		Repo: rp.Name,
		ACLs: acls,
	})
}

func (h *Handler) postAccess(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("repo")
	handle := strings.TrimSpace(r.FormValue("handle"))
	perm := meta.Perm(strings.TrimSpace(r.FormValue("perm")))
	if perm != meta.PermRead && perm != meta.PermWrite && perm != meta.PermAdmin {
		h.renderAccess(w, r, name, "", "permission must be read, write, or admin")
		return
	}
	rp, err := h.md.GetRepo(r.Context(), name)
	if errors.Is(err, meta.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.serverError(w, "get repo", err)
		return
	}
	u, err := h.md.GetUser(r.Context(), handle)
	if errors.Is(err, meta.ErrNotFound) {
		h.renderAccess(w, r, name, "", "no such user: "+handle)
		return
	}
	if err != nil {
		h.serverError(w, "get user", err)
		return
	}
	if err := h.md.SetACL(r.Context(), rp.ID, u.ID, perm); err != nil {
		h.serverError(w, "set acl", err)
		return
	}
	h.renderAccess(w, r, name, "granted "+string(perm)+" to "+handle, "")
}

func (h *Handler) postRevokeAccess(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("repo")
	uid, err := strconv.ParseInt(r.FormValue("user_id"), 10, 64)
	if err != nil {
		h.renderAccess(w, r, name, "", "bad user id")
		return
	}
	rp, err := h.md.GetRepo(r.Context(), name)
	if errors.Is(err, meta.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.serverError(w, "get repo", err)
		return
	}
	if err := h.md.DeleteACL(r.Context(), rp.ID, uid); err != nil {
		h.serverError(w, "revoke acl", err)
		return
	}
	h.renderAccess(w, r, name, "revoked", "")
}

// --- per-repo ci secrets ---

func (h *Handler) getSecrets(w http.ResponseWriter, r *http.Request) {
	h.renderSecrets(w, r, r.PathValue("repo"), "", "")
}

func (h *Handler) renderSecrets(w http.ResponseWriter, r *http.Request, name, flash, errMsg string) {
	rp, err := h.md.GetRepo(r.Context(), name)
	if errors.Is(err, meta.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.serverError(w, "get repo", err)
		return
	}
	data := secretsData{
		base:    h.base(r, flash, errMsg),
		Repo:    rp.Name,
		Enabled: h.secrets != nil && h.secrets.Enabled(),
	}
	if data.Enabled {
		names, err := h.secrets.ListSecretNames(r.Context(), rp.ID)
		if err != nil {
			h.serverError(w, "list secret names", err)
			return
		}
		data.Names = names
	}
	h.render(w, "secrets.html", data)
}

func (h *Handler) postSecret(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("repo")
	secretName := strings.TrimSpace(r.FormValue("name"))
	value := r.FormValue("value") // not trimmed: edge spaces may matter
	if h.secrets == nil || !h.secrets.Enabled() {
		h.renderSecrets(w, r, name, "", "secrets are disabled: set GITMOTE_CI_SECRET_KEY_V1")
		return
	}
	if value == "" {
		h.renderSecrets(w, r, name, "", "value cannot be empty")
		return
	}
	rp, err := h.md.GetRepo(r.Context(), name)
	if errors.Is(err, meta.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.serverError(w, "get repo", err)
		return
	}
	if err := h.secrets.SetSecret(r.Context(), rp.ID, secretName, value); err != nil {
		// At worst, the error contains the secret name, which is public. It
		// never contains the value.
		h.renderSecrets(w, r, name, "", "set secret: "+err.Error())
		return
	}
	h.renderSecrets(w, r, name, "saved secret "+secretName, "")
}

func (h *Handler) postDeleteSecret(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("repo")
	secretName := strings.TrimSpace(r.FormValue("name"))
	rp, err := h.md.GetRepo(r.Context(), name)
	if errors.Is(err, meta.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.serverError(w, "get repo", err)
		return
	}
	if err := h.secrets.DeleteSecret(r.Context(), rp.ID, secretName); err != nil {
		h.serverError(w, "delete secret", err)
		return
	}
	h.renderSecrets(w, r, name, "deleted secret "+secretName, "")
}

// --- rendering ---

// viewer resolves the optional signed-in user for a request. It returns the
// admin that requireAdmin injected, if present. Otherwise it returns the user of
// the session cookie, or else nil for an anonymous viewer. It is best-effort: a
// missing, expired or invalid cookie gives an anonymous viewer.
func (h *Handler) viewer(r *http.Request) *meta.User {
	if u := userFrom(r.Context()); u != nil {
		return u
	}
	if id, ok := h.sess.verify(r, h.now()); ok {
		if u, err := h.md.GetUserByID(r.Context(), id); err == nil {
			return u
		}
	}
	return nil
}

// userID is the user's id, or 0 for a nil (anonymous) viewer.
func userID(u *meta.User) int64 {
	if u != nil {
		return u.ID
	}
	return 0
}

func (h *Handler) base(r *http.Request, flash, errMsg string) base {
	return h.baseFor(h.viewer(r), flash, errMsg)
}

// baseFor builds the shared page header from a viewer that is already resolved.
// A handler that has looked up the viewer (the dashboard) then does not need to
// look it up twice.
func (h *Handler) baseFor(u *meta.User, flash, errMsg string) base {
	b := base{Flash: flash, Err: errMsg}
	if u != nil {
		b.Me = u.Handle
		b.IsAdmin = u.IsAdmin
	}
	return b
}

func (h *Handler) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, name, data); err != nil {
		h.log.Error("render template", "template", name, "error", err)
	}
}

func (h *Handler) serverError(w http.ResponseWriter, msg string, err error) {
	h.log.Error(msg, "error", err)
	http.Error(w, "internal server error", http.StatusInternalServerError)
}
