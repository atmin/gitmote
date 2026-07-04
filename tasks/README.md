# Tasks — the frontier

Active and upcoming work. One file per unit of work, `Spec → Current → Change →
Verify`, **deleted once it lands** (commits are the record — this list never
becomes a changelog).

## Easy to operate — one portable container, S3 as the single source of truth

The milestone (`gitmote can host itself`) is reached; the next theme is **making
gitmote trivial to run**: `docker run ghcr.io/atmin/gitmote` with a handful of
envs, on a laptop or on Scaleway, identically. Kill it — fine; restart it — it
restores from S3 and continues. Localhost is a first-class deployment.

Two independent chains plus a docs cap — order within a chain matters, the chains
run in parallel:

**Chain A — shrink the surface**
- [minimal-env-replica.md](minimal-env-replica.md) — derive the DB replica from
  the bucket (drop `GITMOTE_DB_REPLICA`); a bucket ⇒ replicate + lease (durable by
  default, `RoleOff`-with-a-bucket retired); collapse db/cache/sock into one
  `GITMOTE_DATA`.
- [minimal-env-secrets.md](minimal-env-secrets.md) — auto-generate + persist
  `GITMOTE_COOKIE_KEY` and (lazily) `WORKER_SECRET` in meta; CI master key stays
  env-only.
- [onboarding-bootstrap.md](onboarding-bootstrap.md) — first-run **auto-bootstrap**
  prints the admin token once to the logs with clear instructions (no setup page,
  no race); `bootstrap --reissue` recovers a lost token. *(needs -secrets for the
  cookie key.)*

**Chain B — the image**
- [public-registry.md](public-registry.md) — publish `ghcr.io/atmin/gitmote` +
  `…/gitmote-runner` publicly; deploy from GHCR; kill the by-hand runner push.
- [dogfood-make.md](dogfood-make.md) — `make image` / `make prod` / `make publish`;
  `make prod` runs the real image against dev MinIO. *(needs -replica for full
  shared state, and -registry for the tags.)*

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
