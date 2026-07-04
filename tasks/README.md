# Tasks — the frontier

Active and upcoming work. One file per unit of work, `Spec → Current → Change →
Verify`, **deleted once it lands** (commits are the record — this list never
becomes a changelog).

**The frontier is currently clear.** The **gitmote can host itself** milestone is
reached: it runs on Scaleway at `gitmote.atmin.net`, deploys on every green master
push (GitHub Actions), survives cold starts (litestream restore), and enforces the
single writer by a lease (s3lite `RoleAuto`), so rolling deploys are safe by
construction.

**CI has landed** — run `.github/workflows` on push, `act` self-hosted on Scaleway
Jobs, logs, status UI, encrypted per-repo secrets. The design and rationale now
live in [../docs/architecture/ci.md](../docs/architecture/ci.md). Known limitation:
the Serverless Job sandbox can't build container images, so gitmote does not
rebuild its own image — deployment stays on GitHub Actions.

Speculative future directions live in [../docs/evolution/](../docs/evolution/).
