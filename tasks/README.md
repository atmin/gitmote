# Tasks — the frontier

Active and upcoming work toward the milestone: **gitmote can host itself.**
One file per unit of work, `Spec → Current → Change → Verify`, **deleted once it
lands** (commits are the record — this list never becomes a changelog).

Ordered by dependency; each task lands in a non-breaking, tested state.

The **gitmote can host itself** milestone is reached: it runs on Scaleway at
`gitmote.atmin.net`, deploys itself on every green master push, survives cold
starts (litestream restore), and enforces the single writer by a lease (s3lite
`RoleAuto`), so rolling deploys are safe by construction. Speculative directions
live in [../docs/evolution/](../docs/evolution/).

**CI — run workflows on push.** Design + locked Stage 0 decisions in the epic
index [16-ci.md](16-ci.md) (the *why* is in
[../docs/evolution/ci-runner.md](../docs/evolution/ci-runner.md)). Split into
dependency-ordered stages, each implementable from fresh context:

- [18-ci-config-discovery.md](18-ci-config-discovery.md) — read `.github/workflows`
  at the pushed SHA; create jobs, or no run / a failed run.
- [19-ci-scaleway-trigger.md](19-ci-scaleway-trigger.md) — Scaleway Serverless Jobs
  client + per-job trigger (no-op when unconfigured).
- [20-ci-scoped-tokens.md](20-ci-scoped-tokens.md) — expiring, repo-scoped,
  read-only tokens (precursor: the runner's clone credential).
- [21-ci-runner.md](21-ci-runner.md) — runner image + internal claim/complete API
  (the execution stage; has an infra spike to resolve first).
- [22-ci-secrets.md](22-ci-secrets.md) — encrypted per-repo secrets store
  (AES-256-GCM + HKDF + versioned keys), injected at trigger.
- [23-ci-status-ui.md](23-ci-status-ui.md) — runs list, run detail, log viewer,
  commit status badge (admin-gated).
- [24-ci-self-deploy.md](24-ci-self-deploy.md) — green master run redeploys
  gitmote; latest-wins guard; GitHub mirror as break-glass.
