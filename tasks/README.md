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

- [13-advertise-without-hydration.md](13-advertise-without-hydration.md) — serve
  the `info/refs` advertisement from s3lite refs (via `packed-refs`) with zero
  object hydration. First, separable slice of bounded hydration; the full
  per-operation closure for data POSTs is deferred (needs a reachability index —
  see [../docs/notes/object-hydration.md](../docs/notes/object-hydration.md)).
