# Dashboard + UI reshape

Part of the **URLs** epic ([README](README.md)); implements the top-level globals
and per-repo management surface of
[../docs/architecture/urls.md](../docs/architecture/urls.md). Last in the chain —
it reshapes the UI onto the routes the earlier tasks established.

## Spec

- **`/` is the dashboard / repo list**, scoped to the viewer: public repos to
  anyone, private repos to those with access, management affordances for admin.
- **Global routes are top-level** (no `/ui/` prefix, no `/-/`): `/login`,
  `/logout`, `/users`, `/tokens`, `/static/…`. `/healthz`, `/version` stay.
- **Per-repo management** pages: `/<repo>/settings` (default branch, visibility
  toggle, rename, delete), `/<repo>/access` (list ACLs; invite a **spectator**
  = `read` or a **collaborator** = `write`; revoke), `/<repo>/secrets`.

## Current

Management lives under `/ui/*` behind `requireAdmin`, `/login` is top-level, and
`GET /{$}` redirects to the repo list ([`webui.go`](../internal/webui/webui.go)).
There is no per-repo settings/access page (ACLs are edited on a global
`/ui/acls`), no visibility control, and no viewer-scoped dashboard — everything
UI is admin-only. Static assets are at `/ui/static/…` and are `alwaysServable`
past the leader gate ([`main.go`](../cmd/gitmote/main.go) `alwaysServable`).

## Change

- **Routes** ([`webui.go`](../internal/webui/webui.go) `Register`): move `/ui/repos`,
  `/ui/users`, `/ui/tokens` → `/`, `/users`, `/tokens`; `/ui/static/` → `/static/`;
  keep `/login`,`/logout`. Fold the global `/ui/acls` into per-repo
  `/<repo>/access`; add `/<repo>/settings`, `/<repo>/secrets`.
- **Dashboard** `GET /`: list repos filtered by the viewer (public + those the
  viewer has an ACL on; all for admin), replacing the redirect.
  [`meta.ListRepos`](../internal/meta/repos.go) exists but returns *all* repos —
  add the viewer/visibility filter (join `visibility` + the viewer's ACLs) rather
  than filtering in the handler.
- **Access page** `/<repo>/access`: view ACLs and add/revoke `read`/`write`/`admin`,
  worded as spectator/collaborator; **settings** exposes the `visibility` toggle
  and default branch (feeds the force-push guard from
  [urls-namespace-access.md](urls-namespace-access.md)).
- **Authz**: management routes stay admin; the dashboard and per-repo read pages
  use repo-read (public → anyone, private → ACL), consistent with the access task.
- **Leader gate** ([`main.go`](../cmd/gitmote/main.go) `alwaysServable`): update the
  static prefix to `/static/`.
- **Templates/nav** ([`internal/webui/templates`](../internal/webui/templates)):
  point every generated link at the new scheme; a signed-out visitor sees public
  repos and a login link, not a 403 wall.
- **Docs**: refresh the Management-UI / Quickstart sections of
  [README.md](../README.md) and [ops.md](../docs/ops.md) to the new URLs.

## Verify

- `/` lists exactly the repos the viewer may see (anonymous → public only; ACL
  user → their repos + public; admin → all).
- `/login`, `/logout`, `/users`, `/tokens`, `/static/…` resolve; old `/ui/*` is
  gone.
- `/<repo>/settings` toggles `private`↔`public` and it takes effect (an anonymous
  clone/browse starts/stops working accordingly).
- `/<repo>/access` invites a spectator (read) and a collaborator (write) and
  revokes them; the granted user's access changes accordingly.
- A non-admin cannot reach `settings`/`users`; a signed-out visitor can browse a
  public repo and reach `/login`.
- `gofmt`/`golangci-lint`/`go test ./...` clean; golden **and** failure paths
  (visibility scoping of the dashboard, non-admin management refusal).
