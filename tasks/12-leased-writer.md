# Leased writer — dissolve the deploy-overlap hazard with s3lite `RoleAuto`

## Spec

Make two-writers **impossible by construction** instead of avoided by procedure.
[safety.md §1](../docs/architecture/safety.md) requires exactly one litestream
writer, today enforced by `max-scale = 1` **plus** a stop-first deploy drain
([ops.md](../docs/ops.md) — deploy). The drain closes the rolling-deploy overlap
*in practice, not by proof* — a request racing the ~20 s drain→swap can re-wake
the old image, and there is a brief write+read outage during handoff.

s3lite v0.1.0 ships a lease (`Role`, `IsLeader`, `OnPromote`/`OnDemote`,
`Generation`; reuses litestream's `s3.Leaser`, conditional-write CAS on the same
Scaleway bucket). Booting the server in **`RoleAuto`** makes an instance a writer
only while it *holds the lease* and a read-only follower otherwise, so a rolling
deploy's old+new overlap can never be two writers — the new instance waits as a
follower until the old releases on SIGTERM, then promotes. This is the
[reader-writer-split](../docs/evolution/reader-writer-split.md) evolution note,
now a concrete piece of work since the primitive exists.

**Scope boundary:** this task adopts the lease at **`max-scale = 1`** to remove
the drain hack and prove handoff. It does **not** raise `max-scale` for read
scaling — s3lite followers serve the *restored snapshot* and poll (writes since
the last sync are dropped, `lease.go`), so serving clones off many stale
followers is a **separate** follow-up gated on s3lite follower freshness. Keep
`max-scale = 1`; the lease's value here is safety + a cleaner deploy, not scale.

## Current

- `cmd/gitmote/main.go` opens metadata with `meta.Open(ctx, metaConfigFromEnv(…))`
  in two places (server `buildGitHandler`, and the `bootstrap` one-shot). No role
  is set → s3lite `RoleOff` → **every instance replicates unconditionally as a
  writer**. `metaConfigFromEnv` maps env → `meta.Config` (no role/lease fields).
- `meta.Config` mirrors a subset of `s3lite.Config` (`LocalPath`, `RestoreFrom`,
  `BackupTo`, `S3`, `Logger`); `meta.Metadata` wraps `*s3lite.DB`. No leader
  concept is surfaced.
- Push entry: `githttp.Handler.serveReceivePack` ([backend.go:122]) runs the
  receive-pack path; the actual write goes through `githttp.Writer`. Nothing
  checks "am I allowed to write?" — the single-instance pin is the only guard.
- Deploy safety is external: `POST /admin/quit` (self-SIGTERM drain,
  `main.go` `adminQuitHandler` + the `deployKey` route) and the CI "Drain the
  running writer" step (curl + sleep 20) in `.github/workflows/ci.yml`, plus the
  `max-scale=1` pin re-asserted every deploy.

## Change

Land in **two non-breaking steps**; step 1 is safe even before the deploy is
simplified (a lone instance in `RoleAuto` just always wins the lease = today's
behaviour, now provably single-writer).

### 1. Boot the server as a leased writer

- **meta:** add `Role`, `LeaseTTL`, `Owner` pass-through to `meta.Config` →
  `s3lite.Config`, and surface leader state:
  `func (m *Metadata) IsLeader() bool`, `OnPromote(func())`,
  `OnDemote(func(error))`, `Generation() int64` — thin wrappers over `*s3lite.DB`.
- **main:** set `Role: s3lite.RoleAuto` in `metaConfigFromEnv` for the **server**
  path only (bootstrap stays a deliberate writer — see step-3 note). Leave
  `LeaseTTL`/`Owner` at defaults (30 s TTL, generated owner) unless tuning shows
  otherwise.
- **Constraint to honour:** s3lite swaps the underlying `*sql.DB` on
  promote/demote, so consumers **must route every query through `*s3lite.DB`**
  and never cache the embedded `*sql.DB` across a promotion (s3lite docs,
  `s3lite.go:124`). Verify `internal/meta/*.go` holds only `*s3lite.DB` and issues
  each query through it — it does today; keep it that way.

### 2. Gate the write path on leadership

- In `serveReceivePack` (or one guard the receive path funnels through), reject
  when `!md.IsLeader()`: return a git-legible error over the smart-HTTP sideband /
  an HTTP `503` so the client sees "not the writer, retry." Reads (`upload-pack`,
  UI reads, `/healthz`) stay served regardless — a follower clones fine (from its
  snapshot; acceptable during the brief handoff window at `max-scale=1`).
- Optionally use `OnPromote`/`OnDemote` to log transitions and flip an
  "accepting writes" flag; `IsLeader()` at request time is the source of truth
  (no cached staleness).

### 3. Retire the drain hack (only after 1–2 verified in prod)

- Remove `POST /admin/quit` + `adminQuitHandler` + the `deployKey` plumbing from
  `main.go`; remove the "Drain the running writer" step and the `GITMOTE_DEPLOY_KEY`
  secret from `.github/workflows/ci.yml` and the env/secret tables in `ops.md`.
  The rolling deploy is now safe unaided: new boots as follower, old releases the
  lease on its graceful SIGTERM `Close` (s3lite releases last, after the final
  sync), new promotes.
- **Bootstrap stays a writer:** keep it opening as a writer that *acquires the
  lease* (`RoleWriter`, or `RoleAuto` — pick and document), so it can never run
  concurrently with a live leader. It already runs while the server is scaled to
  zero; the lease makes that a guarantee, not an assumption. Note the cold-start
  latency: after a *hard* kill, the first writer must wait out the ≤30 s lease TTL
  before acquiring (a graceful exit releases immediately, so this only bites on
  crash).
- Update [safety.md §1](../docs/architecture/safety.md) — replace "stop-first
  drain, closed in practice not by proof" with "leased single writer, safe by
  construction"; update the [reader-writer-split](../docs/evolution/reader-writer-split.md)
  note (promote from speculative to shipped for the writer half; read replicas
  remain the open follow-up). `max-scale` stays 1 with a one-line reason: read
  replicas need fresh followers first.

## Verify

- **Unit — write gate:** a follower (`IsLeader()==false`) rejects receive-pack
  with the chosen error; a leader serves it. Reads succeed in both roles.
- **Integration — mutual exclusion & handoff (local, two instances):** two
  gitmote processes, same MinIO replica, both `RoleAuto` → exactly one accepts
  pushes; kill/gracefully-stop the leader → the follower promotes and the next
  push succeeds, with **no interval where both replicate** (assert via
  `Generation()` / lock owner). Mirror s3lite's own lease tests.
- **e2e — deploy overlap (the point of the task):** simulate the rolling window
  — start instance A (leader, holds a push), start instance B against the same
  replica *before* stopping A; confirm B serves clones but refuses pushes while A
  leads; SIGTERM A; confirm B promotes and accepts the push, and the metadata
  replica is uncorrupted (fsck clean on a fresh restore, as `e2e-restore.sh`).
- **Regression:** `make e2e-local` + `make e2e-restore` still green (cold-start
  restore + lossless graceful stop unchanged); `go test ./...` green.
- **Prod smoke:** after deploying step 1–2, one clean rolling deploy shows the
  new instance promote in logs (`OnPromote`) with the old having released; a push
  during the handoff either succeeds (post-promote) or gets the retryable 503
  (never a corrupt replica).
