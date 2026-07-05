// Package webui serves the authenticated management UI — the operations that
// can't be done over git: create/list repos, mint/revoke tokens, and manage
// per-repo ACLs (docs/architecture/storage.md, the "web UI" component). It also
// serves read-only repo browsing (tree/blob/raw/commits/commit) over the
// materialized bare repo. It is a thin server-rendered layer over the same
// s3lite tables and object store the git path uses; it introduces no new source
// of truth.
//
// Management (create/list repos, tokens, users, ACLs, secrets) is gated on the
// global-admin role (users.is_admin); read-only browsing is gated on repo-read
// (public → anyone, private → an ACL — see authorizeRead). A browser logs in by
// pasting a personal access token once (verified through the same path git auth
// uses); the server then issues a signed, stateless session cookie.
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

// mermaidJS is the vendored, version-pinned mermaid library, embedded and served
// same-origin so diagram rendering needs no external fetch (see static/README.md).
//
//go:embed static/mermaid.min.js
var mermaidJS []byte

// Authenticator verifies a raw personal access token, resolving it to its owner.
// *auth.Guard satisfies it; the login form is the only caller.
type Authenticator interface {
	VerifyToken(ctx context.Context, raw string) (*meta.User, error)
}

// SecretsAdmin manages a repo's CI secrets for the admin panel. Values are
// write-only: only names are ever read back. *secrets.Service satisfies it; nil
// disables the secrets UI.
type SecretsAdmin interface {
	Enabled() bool
	SetSecret(ctx context.Context, repoID int64, name, value string) error
	ListSecretNames(ctx context.Context, repoID int64) ([]string, error)
	DeleteSecret(ctx context.Context, repoID int64, name string) error
}

// nameSegment matches a valid owner or repo path segment.
var nameSegment = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// Handler serves the management UI.
type Handler struct {
	md      *meta.Metadata
	mz      *repo.Materializer
	store   store.Store
	auth    Authenticator
	secrets SecretsAdmin
	sess    *sessions
	tmpl    *template.Template
	log     *slog.Logger
	now     func() time.Time
}

// New builds the UI handler. cookieKey signs session cookies and must be
// non-empty; the auth verifier backs the login form; mz materializes repos for
// the read-only browse pages; objs reads CI log blobs for the run views; secrets
// (may be nil) backs the CI secrets panel.
func New(md *meta.Metadata, mz *repo.Materializer, objs store.Store, a Authenticator, secrets SecretsAdmin, cookieKey []byte, logger *slog.Logger) (*Handler, error) {
	if len(cookieKey) == 0 {
		return nil, errors.New("webui: empty cookie key")
	}
	tmpl, err := template.New("webui").Funcs(template.FuncMap{
		// parentPath is the enclosing directory of a browse path, "" at the root
		// — used only to build the ".." link in the tree view.
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
		// statusColor maps a CI run/job status to a CSS color for its badge.
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
		auth:    a,
		secrets: secrets,
		sess:    &sessions{key: cookieKey},
		tmpl:    tmpl,
		log:     logger,
		now:     time.Now,
	}, nil
}

// Register mounts the UI routes on mux. The dashboard, login, and read-only
// browsing are reachable unauthenticated (viewer-scoped); the global-management
// and per-repo-management routes require a global-admin session.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /login", h.getLogin)
	mux.HandleFunc("POST /login", h.postLogin)
	mux.HandleFunc("POST /logout", h.postLogout)

	// The bare root is the dashboard — a viewer-scoped repo list, not a redirect.
	// GET /{$} matches only the exact root, so it never shadows a repo path;
	// POST /{$} is the admin-only repo-create. Both exist only when the UI does.
	mux.HandleFunc("GET /{$}", h.getDashboard)
	mux.HandleFunc("POST /{$}", h.requireAdmin(h.postRepo))

	// Global management — top-level, admin-only (no /ui/ prefix).
	mux.HandleFunc("GET /users", h.requireAdmin(h.getUsers))
	mux.HandleFunc("POST /users", h.requireAdmin(h.postUser))
	mux.HandleFunc("GET /tokens", h.requireAdmin(h.getTokens))
	mux.HandleFunc("POST /tokens", h.requireAdmin(h.postToken))
	mux.HandleFunc("POST /tokens/revoke", h.requireAdmin(h.postRevokeToken))

	// Per-repo management — segment-2 verbs, admin-only. Enumerated (never a broad
	// /{repo}/{rest...}), so git's own suffixes still fall through to the catch-all.
	mux.HandleFunc("GET /{repo}/settings", h.requireAdmin(h.getSettings))
	mux.HandleFunc("POST /{repo}/settings", h.requireAdmin(h.postSettings))
	mux.HandleFunc("GET /{repo}/access", h.requireAdmin(h.getAccess))
	mux.HandleFunc("POST /{repo}/access", h.requireAdmin(h.postAccess))
	mux.HandleFunc("POST /{repo}/access/revoke", h.requireAdmin(h.postRevokeAccess))
	mux.HandleFunc("GET /{repo}/secrets", h.requireAdmin(h.getSecrets))
	mux.HandleFunc("POST /{repo}/secrets", h.requireAdmin(h.postSecret))
	mux.HandleFunc("POST /{repo}/secrets/delete", h.requireAdmin(h.postDeleteSecret))

	// Read-only browsing. Gated on repo-read (public → anyone, private → an ACL),
	// not on admin — this is what makes spectators and public repos usable; the
	// per-request check lives in each handler via authorizeRead. The repo is a
	// single path segment and the ref lives in the path (greedily resolved), so
	// each content verb is its own enumerated route. Only these fixed verbs are
	// registered — never a broad /{repo}/{rest...} — so git's own /{repo}/info/refs,
	// /{repo}/git-upload-pack, … fall through to the smart-HTTP catch-all at "/"
	// (docs/architecture/urls.md → Implementation seams).
	// Each verb registers a bare form and a "/{rest...}" form: a "/{rest...}"
	// pattern is a subtree, so ServeMux would otherwise 307-redirect the bare
	// "/{repo}/runs" to "/{repo}/runs/". The bare form (empty rest) selects the
	// default branch, or the list for runs.
	mux.HandleFunc("GET /{repo}/tree", h.browseTreeRoute)
	mux.HandleFunc("GET /{repo}/tree/{rest...}", h.browseTreeRoute)
	mux.HandleFunc("GET /{repo}/blob", h.browseBlobRoute)
	mux.HandleFunc("GET /{repo}/blob/{rest...}", h.browseBlobRoute)
	mux.HandleFunc("GET /{repo}/raw", h.browseRawRoute)
	mux.HandleFunc("GET /{repo}/raw/{rest...}", h.browseRawRoute)
	mux.HandleFunc("GET /{repo}/commits", h.browseCommitsRoute)
	mux.HandleFunc("GET /{repo}/commits/{rest...}", h.browseCommitsRoute)
	mux.HandleFunc("GET /{repo}/commit/{sha}", h.browseCommitRoute)
	mux.HandleFunc("GET /{repo}/refs", h.browseRefsRoute)
	mux.HandleFunc("GET /{repo}/runs", h.ciRunsRoute)
	mux.HandleFunc("GET /{repo}/runs/{rest...}", h.ciRunsRoute)
	// The bare repo landing: README + tree at the default branch. One segment, so
	// distinct from git's GET /{repo}/info/refs.
	mux.HandleFunc("GET /{repo}", h.browseLanding)

	// The vendored mermaid library for diagram rendering, served same-origin.
	// Public and non-sensitive (a pinned copy of an open-source library), so it
	// needs no admin session; hard-cached, since the file itself is the version pin.
	mux.HandleFunc("GET /static/mermaid.min.js", h.serveMermaidJS)
}

// serveMermaidJS serves the embedded mermaid library for the browse UI's diagram
// rendering. Trusted, fixed asset — immutable-cached so a browser fetches it once.
func (h *Handler) serveMermaidJS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "max-age=31536000, immutable")
	_, _ = w.Write(mermaidJS)
}

// ctxUser carries the authenticated admin from the middleware to the handler.
type ctxKey int

const userKey ctxKey = 0

func userFrom(ctx context.Context) *meta.User {
	u, _ := ctx.Value(userKey).(*meta.User)
	return u
}

// requireAdmin resolves the session cookie to a current, still-admin user on
// every request. Missing/invalid session → redirect to login (GET) or 401;
// authenticated but not an admin → 403.
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

// --- login / logout ---

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
	// Any valid user may sign in — a spectator/collaborator needs a session to
	// browse the private repos they have access to on the dashboard. Management
	// stays admin-only, enforced per-route by requireAdmin (not here).
	h.sess.issue(w, r, user.ID, h.now())
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *Handler) postLogout(w http.ResponseWriter, r *http.Request) {
	h.sess.clear(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// --- dashboard / repos ---

// getDashboard renders the viewer-scoped repo list at "/": public repos to
// anyone, private repos to a viewer holding an ACL, all repos to an admin. It is
// the unauthenticated landing — a signed-out visitor sees public repos and a
// sign-in link, never a 403 wall.
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
	owner := strings.TrimSpace(r.FormValue("owner"))
	name := strings.TrimSpace(r.FormValue("name"))
	branch := strings.TrimSpace(r.FormValue("default_branch"))

	if !nameSegment.MatchString(owner) || !nameSegment.MatchString(name) {
		h.renderDashboard(w, r, "", "owner and name must be alphanumeric (._- allowed, not leading)")
		return
	}
	// The reserved-name and structural rules live in meta.CreateRepo (one source
	// for routing + validation); its error is surfaced below.
	ownerUser, err := h.md.GetUser(r.Context(), owner)
	if errors.Is(err, meta.ErrNotFound) {
		h.renderDashboard(w, r, "", "no such user for owner: "+owner)
		return
	} else if err != nil {
		h.serverError(w, "lookup owner", err)
		return
	}

	// Flat namespace: the repo is a single path segment (no owner/ prefix).
	// CreateRepo enforces the reserved-name and structural rules.
	repo, err := h.md.CreateRepo(r.Context(), name, branch)
	if err != nil {
		h.renderDashboard(w, r, "", "create repo: "+err.Error())
		return
	}
	// Grant the owner admin on their new repo so it is immediately usable
	// (clone/push) without a separate ACL step, mirroring bootstrap.
	if err := h.md.SetACL(r.Context(), repo.ID, ownerUser.ID, meta.PermAdmin); err != nil {
		h.serverError(w, "grant owner acl", err)
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

// postSettings applies whichever settings the form carries: a non-empty
// default_branch and/or a visibility value. A form with neither is a no-op.
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

// renderTokens lists tokens for the selected user (query/form "user"). newRaw,
// when set, is the just-minted token shown exactly once.
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
	value := r.FormValue("value") // not trimmed: a secret may have meaningful edges
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
		// The error names the (public) secret name at worst, never the value.
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

// viewer resolves the optional signed-in user for a request: the admin injected
// by requireAdmin if present, else the session cookie's user, else nil
// (anonymous). Best-effort — a missing/expired/invalid cookie is anonymous.
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

// baseFor builds the shared page header from an already-resolved viewer, so a
// handler that looked the viewer up (the dashboard) needn't do it twice.
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
