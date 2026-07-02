# 10 — Self-host, locally (MinIO + Docker)

Depends on: 01–09.

## Spec

Prove gitmote hosts itself in a local, reproducible environment: the real
gitmote source repo pushed to, and cloned from, a running gitmote container
backed by MinIO. Exercises both request flows and the safety model end-to-end.

## Current

Components pass their own tests, but nothing runs them together against real
storage as one system.

## Change

- `docker-compose.yml` — MinIO + gitmote (single writer), seeded bucket.
- `Dockerfile` — build the static binary; stock `git` present in the image.
- `make e2e-local` — bring up the stack, run `bootstrap`, create the `gitmote`
  repo, `git push` this working tree to it, `git clone` it into a temp dir, and
  assert the clone matches (same `HEAD`, `git fsck` clean, identical file tree).

## Verify

- `make e2e-local` is green locally and in CI (a tagged / gated job).
- Kill + restart the container mid-life: after restart the repo is still fully
  clonable (durability via s3lite / litestream).
- Non-breaking: adds an e2e target; unit gates unchanged.
