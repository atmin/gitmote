# 11 — Deploy: host itself for real

Depends on: 10.

## Spec

Run a single-writer gitmote against real object storage so the gitmote repo
genuinely lives on it. Honor the one operational rule — **never two writer
instances** ([safety.md](../docs/architecture/safety.md) §1).

Target (chosen): **Scaleway Serverless Containers + Scaleway Object Storage**,
`fr-par`, reachable at `gitmote.atmin.net` (CNAME → the container endpoint, auto
TLS). Same host and account as atmin.net — reuse its registry (`rg.fr-par.scw.cloud/atmin`)
and container namespace. See atmin.net's `docs/ops.md` for the pattern.

## Current

Only local (MinIO) hosting exists (task 10). `meta.Config` already exposes
`RestoreFrom` / `BackupTo` / `S3`, but `cmd/gitmote/main.go` passes only
`LocalPath` on both the server and `bootstrap` paths — litestream is unwired.

## Change

### 1. Wire the metadata replica (the only Go change)

`cmd/gitmote/main.go`: a `metaConfigFromEnv()` helper builds `meta.Config` from
env and is used by **both** `buildGitHandler` and `runBootstrap` (today they each
inline `meta.Config{LocalPath: …}`):

- `LocalPath`  ← `GITMOTE_DB`
- `RestoreFrom` = `BackupTo` ← `GITMOTE_DB_REPLICA` (e.g. `s3://gitmote/meta`);
  **empty ⇒ local-only**, so task-10 `make e2e-local` is unchanged.
- `S3.Endpoint` ← `GITMOTE_S3_ENDPOINT`; region/credentials fall through to the
  AWS default chain (`AWS_REGION`, `AWS_ACCESS_KEY_ID`, …) — the same source the
  object store already uses, so one credential set covers objects **and** WAL.

`RestoreFrom` is ignored when `LocalPath` exists, so a warm instance keeps
replicating and a cold instance (fresh ephemeral disk) restores — exactly the
scale-to-zero → wake path.

### 2. De-risk litestream locally before trusting Scaleway

Task 10 never exercised litestream (local-only DB on a volume). Add a
`make e2e-restore` (or extend `scripts/e2e-local.sh`) that sets
`GITMOTE_DB_REPLICA=s3://gitmote/meta` against MinIO, pushes, then **wipes the DB
volume *and* recreates the container**, and asserts the clone still succeeds —
proving the refs came back from S3, not disk. This makes the Scaleway cold-start
verify a confirmation, not a first attempt.

### 3. Image & registry

Dockerfile is arch-agnostic; Scaleway needs `linux/amd64`, so build with
`--platform=linux/amd64` (documented, done in CI). Push to
`rg.fr-par.scw.cloud/atmin/gitmote`.

### 4. Single-writer enforcement (the crux)

- **`max-scale=1`** on the container — a correctness precondition (litestream to
  a shared WAL from two writers can corrupt the replica; this is stronger than
  atmin.net's stateless case). Assert the value at deploy time, not a console
  default.
- **No deploy overlap.** Scaleway's default rollout can briefly run old+new = two
  writers. Investigate whether a stop-before-start strategy exists; if not, the
  deploy **scales to 0, waits for termination, updates the image, scales back to
  1** — a few seconds of downtime traded for the invariant. This is acceptable
  for a personal git host and is the safe default.
- Cross-link the rule from `docs/ops.md` to safety.md §1.

### 5. Bootstrap without a second writer

Run `gitmote bootstrap` **once from the operator's machine** with
`GITMOTE_DB_REPLICA` pointed at the prod bucket, **before** the server scales up
(the operator is transiently the single writer; it writes + flushes replication
on `Close`, the server then restores). Documented in ops.md — do **not** run it
as a job while the server is live.

### 6. Runtime env / secrets (Scaleway container)

`GITMOTE_S3_BUCKET=gitmote`, `GITMOTE_S3_ENDPOINT=https://s3.fr-par.scw.cloud`,
`GITMOTE_S3_PREFIX=objects/`, `GITMOTE_DB_REPLICA=s3://gitmote/meta`,
`GITMOTE_DB=/tmp/gitmote/meta.sqlite3`, `GITMOTE_CACHE=/tmp/gitmote/cache`,
`GITMOTE_SOCK=/tmp/gitmote/gitmote.sock`, `GITMOTE_COOKIE_KEY=<secret>` (UI),
`AWS_REGION=fr-par`, `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY=<Scaleway keys>`
(secret). Memory: start ~**512 MB** (git `index-pack`/`receive-pack` need far
more than atmin.net's 128 MB) and tune from logs; ensure ephemeral scratch for
materialization.

### 7. CI deploy pipeline

Minimal `deploy.yml`: on every green **master push**, build+push the amd64 image
and redeploy the single prod container (`gitmote.atmin.net`) via the
single-writer-safe strategy above, asserting `max-scale=1`. No staging, no tags —
one container, one bucket, one writer. GitHub secrets `SCW_*` as in atmin.net.

### 8. Docs

- **New `docs/ops.md`** — canonical infra doc for gitmote: infra table, the
  single-writer invariant (link safety.md §1) + the deploy strategy, env-var
  table, one-time Scaleway setup (`scw` commands, bucket, CNAME), bootstrap
  ordering, cold-start restore, troubleshooting. Note the EU-resident stance and
  that Scaleway's missing conditional-writes don't affect gitmote (CAS is in
  s3lite, not S3).
- `docs/architecture/storage.md` stays provider-agnostic; the litestream target
  being real is captured in ops.md.

## Verify

- `make e2e-restore` green locally (litestream restore after wiping the DB).
- On the deployed instance: `gitmote bootstrap`, then `git push` the gitmote repo
  over **HTTPS** to `gitmote.atmin.net` and `git clone` it back — the repo lives
  there.
- Cold start (scale-to-zero → wake, or a redeploy) restores metadata and serves
  the repo.
- The deploy pipeline demonstrably never runs two writers concurrently
  (stop-then-start; `max-scale=1` asserted).
- Milestone: **gitmote hosts itself.**
