# Tasks — the frontier

Active and upcoming work toward the milestone: **gitmote can host itself.**
One file per unit of work, `Spec → Current → Change → Verify`, **deleted once it
lands** (commits are the record — this list never becomes a changelog).

Ordered by dependency; each task lands in a non-breaking, tested state.

- [06-auth-pat-acl.md](06-auth-pat-acl.md) — Bearer PATs + per-repo ACL enforcement.
- [07-write-path-cas.md](07-write-path-cas.md) — `receive-pack` + pre-receive RPC + object PUT + ref CAS; `git push` works.
- [08-bootstrap.md](08-bootstrap.md) — First admin, token, repo from an empty bucket.
- [09-management-web-ui.md](09-management-web-ui.md) — Authenticated UI: repos, tokens, ACLs.
- [10-self-host-local.md](10-self-host-local.md) — MinIO + Docker; e2e clone+push of the gitmote repo.
- [11-deploy.md](11-deploy.md) — Real object storage + single-writer deploy; hosts itself for real.

Milestone is done when **10** and **11** are green: the gitmote repo lives on gitmote.
