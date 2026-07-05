# Tasks — the frontier

Active and upcoming work. One file per unit of work, `Spec → Current → Change →
Verify`, **deleted once it lands** (commits are the record — this list never
becomes a changelog).

The **"easy to operate"** theme has fully landed: auto-generated secrets +
first-run auto-bootstrap (shrink the surface), public GHCR images with
`make image`/`prod`/`publish` (the image), and the `docker run` quickstart + env /
CI-substrate docs (the cap). `docker run ghcr.io/atmin/gitmote` with a bucket and
credentials is now a working forge; kill/restart restores from S3.

## Later

- **URL redesign** — unify `/-/tree/` vs `/-/blob/` (or rewrite relative links in
  rendered markdown) so in-repo markdown links resolve correctly and carry the
  ref. (A relative link on a tree page currently resolves to `/-/tree/<file>` →
  "empty tree".)

Speculative directions live in [../docs/evolution/](../docs/evolution/).
