# Flat repo namespace + visibility & access

Part of the **URLs** epic ([README](README.md)); implements the namespace and
access sections of [../docs/architecture/urls.md](../docs/architecture/urls.md).
First in the chain — everything else routes off single-segment repos.

## Spec

- Repos are a **single path segment** (`gitmote`, not `atmin/gitmote`). Reachable
  at `/<repo>` for git. Reserved names (enforced at create): the top-level globals
  `login`, `logout`, `users`, `tokens`, `settings`, `new`, `search`, `api`,
  `metrics`, `internal`, `static`, `healthz`, `version`; plus two structural
  rules — a name may not start with `.` or be the bare `-` (nor be empty). A
  trailing `.git` is stripped. (`internal` is already the CI report-API route;
  the rest are reserved ahead of need — the `.`/`-` rules keep future system
  routes possible without a breaking rename.)
- **Visibility** per repo: `private` (default) or `public`. A public repo is
  readable with **no token** (clone/fetch/browse); writes and management are
  **never** anonymous.
- The ACL perms already exist — **`read` = spectator, `write` = collaborator,
  `admin`**. Reading is gated by **repo-read** (public → anyone, private → a
  `read` ACL), not by global admin.
- **Force-pushing the default branch is admin-only.** A `write` collaborator may
  fast-forward any branch and force-push/delete *non-default* branches, but a
  **non-fast-forward or deletion of the default branch requires `admin`**.
- Greenfield: no users yet, so **reset** rather than migrate.

## Current

`repos.name` is `owner/repo`, `UNIQUE` with an embedded slash
([`schema.sql`](../internal/meta/schema.sql), [`repos.go`](../internal/meta/repos.go)).
The git handler and writer route on `/<owner>/<repo>/…`
([`internal/githttp`](../internal/githttp), [`cmd/gitmote/main.go`](../cmd/gitmote/main.go)).
[`auth.Guard.Authorize`](../internal/auth/guard.go) requires a valid token + a
matching ACL for every request; there is no anonymous path and no `visibility`
column. Browse is **admin-only** ([`webui.go`](../internal/webui/webui.go),
`h.requireAdmin(h.browse)`). Bootstrap and the dev/e2e scripts use `atmin/gitmote`.

## Change

- **Reset (greenfield).** Wipe the bucket + meta and re-bootstrap a single repo
  `gitmote`; no migration path is written. Locally that's `make dev-reset`; in prod
  it's dropping the bucket's `objects/` and `meta` prefixes, then re-bootstrapping
  ([ops.md](../docs/ops.md)). Because the DB is fresh, the schema change is
  `schema.sql` only — no guarded `migrate()` entry needed.
- **Schema** ([`schema.sql`](../internal/meta/schema.sql)): `repos.name` stays
  `UNIQUE` but now holds a single segment; add
  `visibility TEXT NOT NULL DEFAULT 'private' CHECK (visibility IN ('private','public'))`.
- **Repo-name validation** in `CreateRepo` ([`repos.go`](../internal/meta/repos.go)):
  reject a `/`, any reserved global (see Spec), a leading `.`, the bare `-`, and
  empty; strip a trailing `.git`. Keep the reserved set in one place (a package
  var) so routes and validation can't drift. Add `GetRepo`-side `.git` tolerance
  so `clone host/<repo>.git` resolves.
- **Routing**: parse `/<repo>/…` (drop the owner segment) in
  [`internal/githttp`](../internal/githttp) and the mux wiring in
  [`main.go`](../cmd/gitmote/main.go). The git handler keeps the `/` catch-all;
  the UI verb routes ([urls-content-routing.md](urls-content-routing.md)) share
  that space, so **only enumerated verbs** may be registered — never a broad
  `/{repo}/{rest...}` — or git's `info/refs` / `*-pack` endpoints get swallowed
  (see the mux seam in [../docs/architecture/urls.md](../docs/architecture/urls.md)).
- **Authz** ([`guard.go`](../internal/auth/guard.go)): when `perm == read` and the
  repo is `public`, allow with no token; otherwise the existing token+ACL path.
  `write`/`admin` are unchanged (never anonymous). Add a `visibility`-aware repo
  read the UI and git-read paths share.
- **Default-branch force-push guard** in the **pre-receive hook** path (per-ref, so
  the client gets a clean rejection — not a post-hoc CAS failure;
  [`internal/githttp`](../internal/githttp), [`internal/hookrpc`](../internal/hookrpc)).
  A non-fast-forward or deletion of `repos.default_branch` is rejected unless the
  pusher holds `admin`; fast-forwards and all pushes to other branches need only
  `write`. Three inputs must reach that point: the pusher's **ACL level** (admin vs
  write — `classify` today only gates ≥`write`, so admin must be resolved
  separately, e.g. `GetACL`/`is_admin`), the repo's `default_branch`, and per-ref
  **non-ff / delete** detection (old→new ancestry; delete = all-zeros new SHA). The
  rejection is a real refusal, not a silent drop.
- **Browse authz** ([`webui.go`](../internal/webui/webui.go)): gate browse/read on
  repo-read (public → anyone, private → `read` ACL), replacing `requireAdmin` on
  the read routes; management routes stay admin.
- **Bootstrap** ([`internal/bootstrap`](../internal/bootstrap),
  [`main.go`](../cmd/gitmote/main.go)): default/require a single-segment repo name;
  update the dev/e2e scripts and README/ops examples to `gitmote`.
- **Docs**: reconcile [storage.md](../docs/architecture/storage.md) (repos.name
  example), [auth.md](../docs/architecture/auth.md) (visibility + anonymous read),
  and drop the `owner/repo` phrasing from [urls.md](../docs/architecture/urls.md)'s
  Status once landed.

## Verify

- `git clone/push http://<host>/gitmote` works (no owner segment).
- **Public repo:** anonymous `clone`/`fetch` and browse succeed; anonymous push is
  rejected; a `write`-ACL user can push.
- **Private repo:** anonymous read is refused; a `read`-ACL user (spectator) can
  clone/browse but not push; a `write`-ACL user (collaborator) can push.
- **Force-push guard:** a `write` collaborator's fast-forward to the default
  branch succeeds; their **non-fast-forward** to it is **refused**; the same
  force-push to a non-default branch succeeds; an `admin` can force-push the
  default branch. (Golden + failure both tested — a force that isn't refused is a
  bug, per CONTRIBUTING.)
- Creating a repo named `login`/`api`/`internal` (any reserved global), one
  starting with `.`, the bare `-`, containing `/`, or empty is rejected with a
  clear error; `gitmote.git` normalizes to `gitmote`.
- `gofmt`/`golangci-lint`/`go test ./...` clean; golden **and** failure paths
  (anonymous-write refusal, private-read refusal, reserved-name rejection).
