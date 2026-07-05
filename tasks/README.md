# Tasks — the frontier

Active and upcoming work. One file per unit of work, `Spec → Current → Change →
Verify`, **deleted once it lands** (commits are the record — this list never
becomes a changelog).

The **"easy to operate"** theme has fully landed: auto-generated secrets +
first-run auto-bootstrap (shrink the surface), public GHCR images with
`make image`/`prod`/`publish` (the image), and the `docker run` quickstart + env /
CI-substrate docs (the cap). `docker run ghcr.io/atmin/gitmote` with a bucket and
credentials is now a working forge; kill/restart restores from S3.

## URLs, urls, urls

Make a personal instance's URLs simple and correct: a **flat single-segment repo
namespace** (`host/<repo>`, no `owner/`, no `/-/`), content addressed in-path
(`tree`/`blob`/`raw` + greedy ref, no `?ref=` query param), public/private
visibility with spectators & collaborators, and rendered-markdown links that
actually resolve. Greenfield — **reset the bucket + meta**, no migration. Design +
rationale: [../docs/architecture/urls.md](../docs/architecture/urls.md). Order
within the chain matters — the **flat namespace + visibility & access** slice has
landed (single-segment repos, `visibility`, repo-read browse authz, admin-only
default-branch force-push); the rest remain:

- [urls-content-routing.md](urls-content-routing.md) — `tree`/`blob`/`raw` verbs,
  ref-in-path greedy resolution, default-branch landing. *(needs -namespace.)*
- [urls-markdown-links.md](urls-markdown-links.md) — rewrite rendered-markdown
  relative links (nav → `blob`/`tree`) and embeds (→ `raw`), ref preserved.
  *(needs -content-routing.)*
- [urls-dashboard-ui.md](urls-dashboard-ui.md) — `/` dashboard, top-level globals
  (`/login`, `/users`, …), per-repo `settings`/`access`/`secrets`. *(last —
  reshapes the UI onto the new routes.)*

Speculative directions live in [../docs/evolution/](../docs/evolution/).
