# 11 — Deploy: host itself for real

Depends on: 10.

## Spec

Run a single-writer gitmote against real object storage so the gitmote repo
genuinely lives on it. Honor the one operational rule — **never two writer
instances** ([safety.md](../docs/architecture/safety.md) §1).

## Current

Only local (MinIO) hosting exists (task 10).

## Change

- Provision real S3-compatible storage + a single-instance runtime; no
  overlapping deploys of the writer (document the rule and enforce it in the
  deploy config).
- Point s3lite's litestream WAL target at the real bucket; verify restore on
  cold start.
- TLS terminated at the edge; config/secrets via env.
- `docs/ops.md` — the canonical infra doc: resources, env vars, deploy commands,
  the single-writer rule. Keep it updated whenever infra/env changes.

## Verify

- Bootstrap on the deployed instance; `git push` the gitmote repo over HTTPS and
  `git clone` it back — the repo lives there.
- Cold start (scale-to-zero → wake) restores metadata and serves the repo.
- The deploy pipeline never runs two writers concurrently.
- Milestone: **gitmote hosts itself.**
