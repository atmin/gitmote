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

- [14-browse-repo.md](14-browse-repo.md) — read-only repo browsing in the web UI
  (admin-gated): tree, blob, raw download, commit log, single-commit diff, ref
  switcher — all cheap `git` plumbing over the already-materialized repo. No
  syntax highlighting / markdown rendering yet.
