# Tasks — the frontier

Active and upcoming work toward the milestone: **gitmote can host itself.**
One file per unit of work, `Spec → Current → Change → Verify`, **deleted once it
lands** (commits are the record — this list never becomes a changelog).

Ordered by dependency; each task lands in a non-breaking, tested state.

The **gitmote can host itself** milestone is reached: it runs on Scaleway at
`gitmote.atmin.net`, deploys itself on every green master push, and survives cold
starts (litestream restore). Speculative directions live in
[../docs/evolution/](../docs/evolution/).

- [12-leased-writer.md](12-leased-writer.md) — adopt s3lite `RoleAuto` so the
  writer is a lease-holder: two-writers becomes impossible by construction,
  retiring the stop-first deploy drain. (`max-scale=1` stays; read replicas are a
  later follow-up gated on follower freshness.)
