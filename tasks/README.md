# Tasks — the frontier

Active and upcoming work toward the milestone: **gitmote can host itself.**
One file per unit of work, `Spec → Current → Change → Verify`, **deleted once it
lands** (commits are the record — this list never becomes a changelog).

Ordered by dependency; each task lands in a non-breaking, tested state.

- [11-deploy.md](11-deploy.md) — Real object storage + single-writer deploy; hosts itself for real.

Milestone is done when **11** is green: the gitmote repo lives on gitmote for real
(local self-hosting via `make e2e-local` already passes).
