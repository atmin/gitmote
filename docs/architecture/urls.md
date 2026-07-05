# URLs

gitmote is a **personal instance**: one owner, a handful of repos, a few invited
people. That single fact sets the whole URL shape — repos live in a **flat,
single-segment namespace** (`gitmote.example.com/<repo>`), not `user/repo`. There
is no per-user namespace to disambiguate, so there is nothing to disambiguate.

> **Status:** partially landed. The **flat single-segment namespace** and
> **access & visibility** (public → anonymous read, repo-read browse, admin-only
> default-branch force-push) are implemented and reconciled into
> [storage.md](storage.md) and [auth.md](auth.md); the **`tree`/`blob`/`raw`
> content routing** (ref-in-path greedy resolution, unified `tree`, self-healing
> `blob`, the bare `/{repo}` landing) is implemented per the sections below. Still
> to land from the `tasks/urls-*` chain: rendered-markdown link rewriting, and the
> dashboard UI on the new routes.

## The namespace

Repos are a single path segment. Everything that is *not* repo content is either a
verb under the repo or a top-level global route:

| What | URL | Access |
|------|-----|--------|
| Dashboard / repo list | `/` | per viewer |
| Repo landing (README + tree @ default branch) | `/<repo>` (+ git's own suffixes) | read |
| Clone / fetch | `GET /<repo>/info/refs?service=git-upload-pack`, `POST /<repo>/git-upload-pack` | read |
| Push | `POST /<repo>/git-receive-pack` | write |
| Browse (dir or file) | `/<repo>/tree/<ref>/<path…>` | read |
| File (explicit) | `/<repo>/blob/<ref>/<path…>` | read |
| Raw bytes | `/<repo>/raw/<ref>/<path…>` | read |
| History / one commit | `/<repo>/commits/<ref>/<path…>`, `/<repo>/commit/<sha>` | read |
| Branches / tags | `/<repo>/refs` | read |
| CI runs / one run | `/<repo>/runs`, `/<repo>/runs/<id>` | read |
| Settings (default branch, visibility, rename, delete) | `/<repo>/settings` | admin |
| Access (invite / revoke collaborators & spectators) | `/<repo>/access` | admin |
| CI secrets | `/<repo>/secrets` | admin |
| Login / logout | `/login`, `/logout` | — |
| Users | `/users` | admin |
| Personal access tokens | `/tokens` | signed-in |
| Static assets | `/static/…` | — |
| Liveness / version | `/healthz`, `/version` | — |

**Reserved names are a small fixed set** — the top-level globals `login`,
`logout`, `users`, `tokens`, `settings`, `new`, `search`, `api`, `metrics`,
`internal`, `static`, `healthz`, `version` — plus two structural rules: a repo
name may not **start with `.`** and may not be the bare **`-`** (nor be empty). A
name is rejected against this at creation with a clear error. `internal` is
*already* live (the CI report API, [request-flows.md](request-flows.md)); the rest
are reserved ahead of need, because un-reserving later — once a repo owns the name
— is a breaking rename, while reserving costs the single owner nothing.

Nothing else is reserved: the content/action verbs (`tree`, `blob`, `raw`,
`commits`, `runs`, …) sit at **segment 2**, so a repo *named* `tree` is legal
(`/tree/tree/main/x` = repo `tree`, verb `tree`, ref `main`, path `x`). Git's own
segment-2 suffixes (`info`, `git-upload-pack`, `git-receive-pack`, `HEAD`,
`objects`) and the UI verbs are disjoint fixed vocabularies.

Repo-name rules: a single segment, no `/`; not reserved (above); a trailing
`.git` is stripped (`clone host/gitmote.git` == `host/gitmote`).

## Content addressing

Three content verbs, and the **ref lives in the path, never a query parameter**:

- **`tree/<ref>/<path>`** — canonical and unified. A directory renders a listing;
  a file renders its view (markdown / highlighted source). The server detects
  which from the tree.
- **`blob/<ref>/<path>`** — explicit "this is a file": renders the file. If the
  path is actually a directory it **301s to `tree/<ref>/<path>`** (canonicalize,
  don't 404). So `blob` self-heals and `tree` never guesses wrong.
- **`raw/<ref>/<path>`** — the bytes (file only; a directory is 404).

The UI and the markdown rewriter **emit `blob` for files and `tree` for
directories** (self-describing, GitHub-familiar); both verbs stay tolerant as
above so a hand-written or external link with the "wrong" verb still resolves.

**Ref resolution is greedy.** `<ref>` and `<path>` both may contain slashes, so
the server takes the **longest leading path prefix that names a real ref**
(branch or tag from the materialized repo). `/<repo>/tree/feature/login/src/x.go`
with a branch `feature/login` → ref `feature/login`, path `src/x.go`. An omitted
ref (the bare `/<repo>` landing) means the repo's default branch.

## Relative links in rendered content

Ref-in-path is what makes relative links work: a link from
`/<repo>/tree/main/docs/readme.md` resolves against that URL and keeps `<repo>`
and `<ref>` for free. But two things a bare relative link can't carry, the
renderer must supply, so **rendered markdown has its relative links rewritten**:

- **Embedded assets** (`![](diagram.png)`) must point at `raw` bytes, not an HTML
  view — the browser can't infer that, so images/embeds are rewritten to
  `/<repo>/raw/<ref>/…`.
- **Navigation links** are rewritten to the precise verb (`blob` for a file,
  `tree` for a directory, decided by a tree lookup) with the current ref baked in.

Absolute URLs, external `http(s)` links, and in-page anchors (`#…`) are left
untouched.

## Access & visibility

Users exist so the owner can invite people; access is per-repo, read on every
request from the `acls` table ([auth.md](auth.md)):

- **`read` = spectator**, **`write` = collaborator**, **`admin`** = repo
  management. The "invite" flows are just *create/select user + grant the ACL* —
  no new perm model.
- **`repos.visibility`** is `private` (default) or `public`. A **public** repo is
  readable with **no token** (clone, fetch, and browse). Writes are **never**
  anonymous — `git-receive-pack` and all management always require a `write`/
  `admin` ACL, public or not.
- Reading (browse + fetch) is gated by **repo-read** — public → anyone, private →
  a `read` ACL — *not* by global admin. (Browse is admin-only today; this opens
  it up, which is what makes spectators and public repos usable.)
- **Force-pushing the default branch is admin-only.** A `write` collaborator may
  fast-forward any branch (including the default) and may force-push or delete
  *non-default* branches, but a **non-fast-forward or deletion of the default
  branch requires `admin`**. History on the branch everyone tracks is protected
  from a collaborator's rewrite; the ordinary day-to-day push (a fast-forward) is
  unaffected. This is enforced on the write path, where the old→new relationship
  is known (the ref CAS / pre-receive hook, [safety.md](safety.md)), not at the
  initial request authorization.

Per-repo *ownership* leaves the namespace entirely: repos are instance-level, the
admin implicitly owns them, and ACLs grant everyone else. That is the core
simplification the flat namespace buys.

## Implementation seams

Two non-obvious constraints the flat scheme creates:

- **The browse routes share the mux with the git smart-HTTP catch-all.** Today the
  UI is quarantined (browse under `/browse/…`, root as `GET /{$}`); the git handler
  owns the `/` catch-all ([request-flows.md](request-flows.md)). Moving browse into
  `/{repo}/…` puts it beside git's `info/refs`, `git-upload-pack`,
  `git-receive-pack`, `HEAD`, and `objects/`. The rule that keeps clone/push
  working: **register only enumerated verb routes** (`GET /{repo}`,
  `GET /{repo}/tree/…`, `/{repo}/blob/…`, …) — **never** a broad
  `/{repo}/{rest...}`, which would swallow the git endpoints. Go's ServeMux
  (most-specific-wins) then routes verbs to the UI and everything else on
  `/{repo}/…` to the git catch-all; the one-segment `GET /{repo}` landing is
  distinct from `GET /{repo}/info/refs`.
- **The default-branch force-push guard lives in the pre-receive hook.** It must
  reject *per ref* so the client sees a clean "force-push to `main` requires admin"
  — not a post-hoc failure. That point needs three inputs threaded to it: the
  pusher's **ACL level** (admin vs write — the write path otherwise only
  distinguishes ≥`write`), the repo's `default_branch`, and per-ref
  **non-fast-forward / deletion** detection (old→new ancestry; a delete is a
  new-SHA of all-zeros).

## Why it looks like this

- **No `/-/` separator.** GitLab needs `/-/` because it has arbitrarily nested
  groups (`group/sub/…/project`), so the project↔verb boundary is ambiguous.
  GitHub, with a fixed `owner/repo`, needs no marker. gitmote's flat
  single-segment repo is even simpler — the verb is always segment 2 — so a
  marker would buy nothing. Dropped everywhere.
- **A reserved-name *list* is a multi-tenant problem.** It bites GitHub because
  users pick names it doesn't control (hence its reserved-username list). On a
  personal instance the owner names every repo, so a small fixed reserved set
  (the globals above) is a non-issue — you simply never name a repo `login`.
- **The `.`/`-` rules are escape hatches, not features.** Reserving names that
  start with `.` and the bare `-` costs the owner nothing but guarantees room for
  the unforeseen: a future well-known route lives under `.well-known/`, and a
  future system namespace can reuse `-/…` — neither forcing a breaking rename of
  an existing repo. That's why the word-list only needs names we want *pretty and
  top-level*; everything else can hide behind a hatch. Reserving ahead is cheap
  insurance; un-reserving after a repo claims a name is not.
- **Greedy refs are safe.** The scary ambiguity (is `a/b` the ref `a` or `a/b`?)
  mostly can't occur: git's ref store forbids a branch `a` and a branch `a/b`
  from coexisting (a name can't be both a file and a directory under
  `refs/heads`). The only residual case is a branch and a *tag* sharing a prefix,
  resolved by a fixed precedence (branch wins). Pretty URLs, no `%2F` encoding
  and its proxy/`net/http` footguns.
- **Unify + tolerant `blob`.** One content verb means a relative link resolves
  correctly whether the target is a file or a directory, without the link author
  knowing which; `blob`/`tree` tolerance means an imprecise link still lands
  somewhere valid instead of 404ing.
- **Ref in the path, not the query.** A query-param ref (`?ref=master`) is dropped
  by relative links — the concrete cause of today's "empty tree" on an in-repo
  markdown link. In the path, it survives relative resolution and is shareable.
