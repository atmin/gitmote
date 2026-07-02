// Package webui serves the authenticated management UI — the operations that
// can't be done over git: create/list repos, mint/revoke tokens, and manage
// per-repo ACLs (docs/architecture/storage.md, the "web UI" component). It is a
// thin server-rendered layer over the same s3lite tables the git path uses; it
// introduces no new source of truth.
//
// Access is gated on the global-admin role (users.is_admin). A browser logs in
// by pasting a personal access token once (verified through the same path git
// auth uses); the server then issues a signed, stateless session cookie.
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
)

//go:embed templates/*.html
var templatesFS embed.FS

// Authenticator verifies a raw personal access token, resolving it to its owner.
// *auth.Guard satisfies it; the login form is the only caller.
type Authenticator interface {
	VerifyToken(ctx context.Context, raw string) (*meta.User, error)
}

// reservedOwners are top-level path segments the server already routes; a repo
// whose owner is one of these would shadow those routes, so creation refuses it.
var reservedOwners = map[string]bool{
	"ui": true, "login": true, "logout": true, "healthz": true, "version": true,
}

// nameSegment matches a valid owner or repo path segment.
var nameSegment = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// Handler serves the management UI.
type Handler struct {
	md   *meta.Metadata
	auth Authenticator
	sess *sessions
	tmpl *template.Template
	log  *slog.Logger
	now  func() time.Time
}

// New builds the UI handler. cookieKey signs session cookies and must be
// non-empty; the auth verifier backs the login form.
func New(md *meta.Metadata, a Authenticator, cookieKey []byte, logger *slog.Logger) (*Handler, error) {
	if len(cookieKey) == 0 {
		return nil, errors.New("webui: empty cookie key")
	}
	tmpl, err := template.ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{
		md:   md,
		auth: a,
		sess: &sessions{key: cookieKey},
		tmpl: tmpl,
		log:  logger,
		now:  time.Now,
	}, nil
}

// Register mounts the UI routes on mux. Login/logout are reachable
// unauthenticated; everything under /ui/ requires a global-admin session.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /login", h.getLogin)
	mux.HandleFunc("POST /login", h.postLogin)
	mux.HandleFunc("POST /logout", h.postLogout)

	mux.HandleFunc("GET /ui/{$}", h.requireAdmin(h.redirectRepos))
	mux.HandleFunc("GET /ui/repos", h.requireAdmin(h.getRepos))
	mux.HandleFunc("POST /ui/repos", h.requireAdmin(h.postRepo))
	mux.HandleFunc("POST /ui/repos/default-branch", h.requireAdmin(h.postDefaultBranch))
	mux.HandleFunc("GET /ui/users", h.requireAdmin(h.getUsers))
	mux.HandleFunc("POST /ui/users", h.requireAdmin(h.postUser))
	mux.HandleFunc("GET /ui/tokens", h.requireAdmin(h.getTokens))
	mux.HandleFunc("POST /ui/tokens", h.requireAdmin(h.postToken))
	mux.HandleFunc("POST /ui/tokens/revoke", h.requireAdmin(h.postRevokeToken))
	mux.HandleFunc("GET /ui/acls", h.requireAdmin(h.getACLs))
	mux.HandleFunc("POST /ui/acls", h.requireAdmin(h.postACL))
	mux.HandleFunc("POST /ui/acls/revoke", h.requireAdmin(h.postRevokeACL))
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
	if !user.IsAdmin {
		w.WriteHeader(http.StatusForbidden)
		h.render(w, "login.html", loginData{Err: "not an administrator"})
		return
	}
	h.sess.issue(w, r, user.ID, h.now())
	http.Redirect(w, r, "/ui/repos", http.StatusSeeOther)
}

func (h *Handler) postLogout(w http.ResponseWriter, r *http.Request) {
	h.sess.clear(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// --- repos ---

func (h *Handler) redirectRepos(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/ui/repos", http.StatusSeeOther)
}

func (h *Handler) getRepos(w http.ResponseWriter, r *http.Request) {
	h.renderRepos(w, r, "", "")
}

func (h *Handler) renderRepos(w http.ResponseWriter, r *http.Request, flash, errMsg string) {
	repos, err := h.md.ListRepos(r.Context())
	if err != nil {
		h.serverError(w, "list repos", err)
		return
	}
	h.render(w, "repos.html", reposData{
		base:  h.base(r, flash, errMsg),
		Repos: repos,
	})
}

func (h *Handler) postRepo(w http.ResponseWriter, r *http.Request) {
	owner := strings.TrimSpace(r.FormValue("owner"))
	name := strings.TrimSpace(r.FormValue("name"))
	branch := strings.TrimSpace(r.FormValue("default_branch"))

	if !nameSegment.MatchString(owner) || !nameSegment.MatchString(name) {
		h.renderRepos(w, r, "", "owner and name must be alphanumeric (._- allowed, not leading)")
		return
	}
	if reservedOwners[owner] {
		h.renderRepos(w, r, "", "owner is reserved: "+owner)
		return
	}
	if _, err := h.md.GetUser(r.Context(), owner); errors.Is(err, meta.ErrNotFound) {
		h.renderRepos(w, r, "", "no such user for owner: "+owner)
		return
	} else if err != nil {
		h.serverError(w, "lookup owner", err)
		return
	}

	full := owner + "/" + name
	if _, err := h.md.CreateRepo(r.Context(), full, branch); err != nil {
		h.renderRepos(w, r, "", "create repo: "+err.Error())
		return
	}
	h.renderRepos(w, r, "created "+full, "")
}

func (h *Handler) postDefaultBranch(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("repo"))
	branch := strings.TrimSpace(r.FormValue("default_branch"))
	if branch == "" {
		h.renderRepos(w, r, "", "default branch cannot be empty")
		return
	}
	repo, err := h.md.GetRepo(r.Context(), name)
	if errors.Is(err, meta.ErrNotFound) {
		h.renderRepos(w, r, "", "no such repo: "+name)
		return
	}
	if err != nil {
		h.serverError(w, "get repo", err)
		return
	}
	if err := h.md.SetDefaultBranch(r.Context(), repo.ID, branch); err != nil {
		h.serverError(w, "set default branch", err)
		return
	}
	h.renderRepos(w, r, "default branch of "+name+" set to "+branch, "")
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

// --- acls ---

func (h *Handler) getACLs(w http.ResponseWriter, r *http.Request) {
	h.renderACLs(w, r, strings.TrimSpace(r.FormValue("repo")), "", "")
}

func (h *Handler) renderACLs(w http.ResponseWriter, r *http.Request, repoName, flash, errMsg string) {
	repos, err := h.md.ListRepos(r.Context())
	if err != nil {
		h.serverError(w, "list repos", err)
		return
	}
	data := aclsData{
		base:     h.base(r, flash, errMsg),
		Repos:    repos,
		Selected: repoName,
	}
	if repoName != "" {
		repo, err := h.md.GetRepo(r.Context(), repoName)
		if errors.Is(err, meta.ErrNotFound) {
			data.Err = "no such repo: " + repoName
			h.render(w, "acls.html", data)
			return
		}
		if err != nil {
			h.serverError(w, "get repo", err)
			return
		}
		acls, err := h.md.ListACLs(r.Context(), repo.ID)
		if err != nil {
			h.serverError(w, "list acls", err)
			return
		}
		data.ACLs = acls
	}
	h.render(w, "acls.html", data)
}

func (h *Handler) postACL(w http.ResponseWriter, r *http.Request) {
	repoName := strings.TrimSpace(r.FormValue("repo"))
	handle := strings.TrimSpace(r.FormValue("handle"))
	perm := meta.Perm(strings.TrimSpace(r.FormValue("perm")))
	if perm != meta.PermRead && perm != meta.PermWrite && perm != meta.PermAdmin {
		h.renderACLs(w, r, repoName, "", "perm must be read, write, or admin")
		return
	}
	repo, err := h.md.GetRepo(r.Context(), repoName)
	if errors.Is(err, meta.ErrNotFound) {
		h.renderACLs(w, r, repoName, "", "no such repo: "+repoName)
		return
	}
	if err != nil {
		h.serverError(w, "get repo", err)
		return
	}
	u, err := h.md.GetUser(r.Context(), handle)
	if errors.Is(err, meta.ErrNotFound) {
		h.renderACLs(w, r, repoName, "", "no such user: "+handle)
		return
	}
	if err != nil {
		h.serverError(w, "get user", err)
		return
	}
	if err := h.md.SetACL(r.Context(), repo.ID, u.ID, perm); err != nil {
		h.serverError(w, "set acl", err)
		return
	}
	h.renderACLs(w, r, repoName, "granted "+string(perm)+" to "+handle, "")
}

func (h *Handler) postRevokeACL(w http.ResponseWriter, r *http.Request) {
	repoName := strings.TrimSpace(r.FormValue("repo"))
	userID, err := strconv.ParseInt(r.FormValue("user_id"), 10, 64)
	if err != nil {
		h.renderACLs(w, r, repoName, "", "bad user id")
		return
	}
	repo, err := h.md.GetRepo(r.Context(), repoName)
	if errors.Is(err, meta.ErrNotFound) {
		h.renderACLs(w, r, repoName, "", "no such repo: "+repoName)
		return
	}
	if err != nil {
		h.serverError(w, "get repo", err)
		return
	}
	if err := h.md.DeleteACL(r.Context(), repo.ID, userID); err != nil {
		h.serverError(w, "revoke acl", err)
		return
	}
	h.renderACLs(w, r, repoName, "revoked", "")
}

// --- rendering ---

func (h *Handler) base(r *http.Request, flash, errMsg string) base {
	var me string
	if u := userFrom(r.Context()); u != nil {
		me = u.Handle
	}
	return base{Me: me, Flash: flash, Err: errMsg}
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
