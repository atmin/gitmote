# 02 — S3 object store

Depends on: 01.

## Spec

Durable, content-addressed storage for git objects and packs in S3, per the S3
layout in [storage.md](../docs/architecture/storage.md). PUT is idempotent
(re-PUT of the same hash is a no-op) — the substrate for the content-before-pointer
invariant in [safety.md](../docs/architecture/safety.md).

## Current

Nothing yet.

## Change

- `internal/store/store.go` — the `Store` interface (the storage contract):
  Put / Get / Exists / List over keys under `{repo}/objects/…` and
  `{repo}/objects/pack/…`.
- `internal/store/s3.go` — S3 implementation (`aws-sdk-go-v2`; the arch notes
  the SDK directly, with `rclone` as a zero-code fallback). Config via env.
- `internal/store/mem.go` — in-memory implementation for tests, kept in lockstep
  with the S3 impl (CONTRIBUTING: both satisfy `Store`).
- A shared conformance test suite exercised against both impls.

## Verify

- Conformance suite passes for `mem` always, and for `s3` against MinIO when
  available (gated by env).
- Re-PUT of the same key is idempotent; `Exists`/`List` behave at prefix
  boundaries.
- Non-breaking: a library with no route wired in yet.
