# 05 — Read path: clone / fetch

Depends on: 04.

## Spec

Serve smart-HTTP `git-upload-pack` so `git clone` / `fetch` work, per the read
path in [request-flows.md](../docs/architecture/request-flows.md). No lock; refs
advertised from s3lite, object closure hydrated from S3.

## Current

Only `/healthz` and `/version` exist (task 01).

## Change

- Routes: `GET /{repo}/info/refs?service=git-upload-pack` (advertise from
  s3lite) and `POST /{repo}/git-upload-pack`.
- Materialize (task 04), then delegate protocol work to stock git via
  `net/http/cgi` invoking `git http-backend` (arch: wrap `http-backend`). Set
  `GIT_PROJECT_ROOT` / `PATH_INFO` to the materialized repo.
- Unauthenticated for now — auth lands in task 06.

## Verify

- Against a running server with a seeded repo, a real
  `git clone http://localhost/{repo}` succeeds and `git fsck` on the clone is
  clean; `git fetch` after a seeded update pulls the new commits.
- Non-breaking: adds read-only endpoints; existing routes untouched.
