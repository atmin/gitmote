# Shrink the server env — derive the replica, collapse the data paths

Part of [easy to operate](README.md). Cut required and optional envs so a run is
a handful of vars, and make "stateless, restores-on-restart" the default.

## Spec

- **Derive the replica from the bucket.** With `GITMOTE_S3_BUCKET` set, gitmote
  replicates metadata to S3 and runs the single-writer lease **without a second
  env**. `GITMOTE_DB_REPLICA` becomes an optional override, not a requirement.
- **A bucket means durability, always.** Once the replica is derived, "bucket set
  but not replicating" (`RoleOff` with a bucket) has no real use — ephemeral/test
  runs use the in-memory store (no bucket). So `RoleOff` stops being a
  bucket-mode: a bucket ⇒ `RoleAuto` (replicate + lease). This is a behavior
  change for `make dev` / `e2e-local` (they gain replication + a lease); it is the
  point — it's what lets `make prod` share state and restore from S3.
- **One data dir.** Collapse `GITMOTE_DB` / `GITMOTE_CACHE` / `GITMOTE_SOCK` into
  a single `GITMOTE_DATA` (default a temp dir): db `= $DATA/meta.sqlite3`, cache
  `= $DATA/cache`, sock `= $DATA/gitmote.sock`. One `-v ./data:/data` for the
  whole container. The three specific vars can stay as overrides if trivial, but
  the documented knob is one.
- **Fewer knobs.** Hardcode the listen address to `:8080` — drop `GITMOTE_ADDR`
  (map ports at the container). Retire `GITMOTE_S3_PREFIX` as a documented knob
  (keep the code default `""` so an existing prefixed bucket still works, but it
  leaves the surface — it only mattered for sharing a bucket across apps).

## Current

[`metaConfigFromEnv`](../cmd/gitmote/main.go) sets `Role` only when
`GITMOTE_DB_REPLICA` is non-empty → `RoleOff` otherwise. It's called **twice** —
`RoleWriter` for the bootstrap CLI and `RoleAuto` for the server — so the
derivation lands in one place and fixes both. Prod runs
`GITMOTE_S3_PREFIX=objects/` with the replica at `s3://gitmote/meta` — meta and
objects are **siblings** under the bucket root. Path vars default individually
(`GITMOTE_DB` → `gitmote.sqlite3`, cache/sock → temp).

## Change

- In `metaConfigFromEnv`, when `GITMOTE_DB_REPLICA` is empty but the bucket is
  set, derive `replica = s3://{bucket}/meta`. **Derive from the bucket only — not
  bucket + prefix.** `s3://{bucket}/{prefix}meta` would be a *different* path and
  orphan the existing replica; the sibling layout (`objects/` + `meta`) is
  deliberate. (No prod data is at risk today, but get the layout right so it
  stays migration-free.)
- Drop the `RoleOff`-with-a-bucket path; a bucket always yields `RoleAuto`.
- Add `GITMOTE_DATA` and derive the three paths from it; keep the explicit vars as
  overrides.
- Replace `envOr("GITMOTE_ADDR", ":8080")` with a `:8080` constant (drop the flag
  + env). Stop reading/documenting `GITMOTE_S3_PREFIX` in the ops surface (leave
  `store.NewS3FromEnv`'s default `""` intact for back-compat).
- Update `scripts/dev.sh` (drop the explicit replica + the path vars; use
  `GITMOTE_DATA=data`), and the `docs/ops.md` / `Dockerfile` env docs.

## Verify

- Unit test: derivation is `s3://{bucket}/meta` for any prefix (assert the prefix
  does **not** leak into the replica path); an explicit `GITMOTE_DB_REPLICA` still
  overrides.
- `make e2e-restore` (cold-start restore) and `make e2e-local` stay green with the
  now-default replication.
- `GITMOTE_DATA=./data` alone places db/cache/sock under it.
- `gofmt`/`golangci-lint`/`go test ./...` clean.
