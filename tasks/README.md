# Tasks — the frontier

Active and upcoming work. One file per unit of work, `Spec → Current → Change →
Verify`, **deleted once it lands** (commits are the record — this list never
becomes a changelog).

The **"easy to operate"** theme has fully landed: auto-generated secrets +
first-run auto-bootstrap (shrink the surface), public GHCR images with
`make image`/`prod`/`publish` (the image), and the `docker run` quickstart + env /
CI-substrate docs (the cap). `docker run ghcr.io/atmin/gitmote` with a bucket and
credentials is now a working forge; kill/restart restores from S3.

The **URLs** epic has fully landed: a flat single-segment repo namespace,
`tree`/`blob`/`raw` content routing with in-path greedy refs, rendered-markdown
link rewriting, public/private visibility with spectators & collaborators, and a
viewer-scoped dashboard with top-level globals and per-repo management. See
[../docs/architecture/urls.md](../docs/architecture/urls.md).

No active tasks. Speculative directions live in
[../docs/evolution/](../docs/evolution/).
