# 04 — Repo materialization / cache

Depends on: 02, 03.

## Spec

Produce a valid on-disk **bare** repo for an operation — a disposable cache,
never the source of truth ([storage.md](../docs/architecture/storage.md) →
ephemeral disk). Refs come from s3lite; objects are hydrated from S3 into the
closure the operation needs.

## Current

Nothing yet.

## Change

- `internal/repo/materialize.go` — given a repo name: create a temp bare repo
  (`git init --bare`), write refs from s3lite, hydrate objects from the `Store`.
- **Hydration policy: full-hydrate for the MVP** — a write hydrates the target
  branch's full history (needed for the fast-forward / connectivity check), a
  read hydrates the closure it serves. Partial-clone / promisor is a later
  optimization (see
  [notes/object-hydration.md](../docs/notes/object-hydration.md)); gitmote's own
  repo is tiny, so full-hydrate is the safe, simple first cut.
- Per-repo cache dir with rebuild-on-miss; eviction is best-effort (disposable).

## Verify

- Given a seeded repo (S3 objects + s3lite refs), materialize then assert with
  real git: `git -C <dir> rev-parse <ref>` and `git cat-file --batch-check`
  resolve; `git fsck` is clean.
- A cold rebuild after deleting the cache dir reproduces an equivalent repo.
- Non-breaking: internal helper, no route yet.
