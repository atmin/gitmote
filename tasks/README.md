# Tasks — the frontier

Active and upcoming work. One file per unit of work, `Spec → Current → Change →
Verify`, **deleted once it lands** (commits are the record — this list never
becomes a changelog).

## Easy to operate — one portable container, S3 as the single source of truth

The milestone (`gitmote can host itself`) is reached; the next theme is **making
gitmote trivial to run**: `docker run ghcr.io/atmin/gitmote` with a handful of
envs, on a laptop or on Scaleway, identically. Kill it — fine; restart it — it
restores from S3 and continues. Localhost is a first-class deployment.

Chain A (shrink the surface — auto-generated secrets, first-run auto-bootstrap)
has landed. What remains is the image chain plus a docs cap — order within the
chain matters:

**Chain B — the image**
- [public-registry.md](public-registry.md) — publish `ghcr.io/atmin/gitmote` +
  `…/gitmote-runner` publicly; deploy from GHCR; kill the by-hand runner push.
- [dogfood-make.md](dogfood-make.md) — `make image` / `make prod` / `make publish`;
  `make prod` runs the real image against dev MinIO. *(needs -registry for the
  tags.)*

**Cap**
- [operate-docs.md](operate-docs.md) — `docker run` quickstart (incl. "grab the
  token from the logs") + CI substrate auto-detection docs. *(last — describes the
  final surface.)*

## Later

- **URL redesign** — unify `/-/tree/` vs `/-/blob/` (or rewrite relative links in
  rendered markdown) so in-repo markdown links resolve correctly and carry the
  ref. (A relative link on a tree page currently resolves to `/-/tree/<file>` →
  "empty tree".)

Speculative directions live in [../docs/evolution/](../docs/evolution/).
