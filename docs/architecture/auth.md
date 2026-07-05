# Auth & transport

> Part of the [gitmote architecture](README.md).

- **Smart HTTP first.** `git http-backend` over HTTPS, authenticated with
  bearer **personal access tokens** (hashed in `tokens`). Stateless, firewall-
  friendly, and the natural fit for a scale-to-zero container.
- **Authorization is per-repo**, read from `acls` on every request. The levels
  are `read` (spectator), `write` (collaborator), and `admin` (repo management).
- **Visibility gates reads, not writes.** `repos.visibility` is `private`
  (default) or `public`. A **public** repo is readable — clone, fetch, and
  browse — with **no token**; reading a private repo needs an ACL. Writes and
  management are **never** anonymous: `git-receive-pack` and every management
  action always require the matching `write`/`admin` ACL. See
  [urls.md](urls.md) for the flat namespace this sits in.
- **Force-pushing the default branch is admin-only.** A `write` collaborator may
  fast-forward any branch and force-push or delete *non-default* branches, but a
  non-fast-forward or deletion of `repos.default_branch` requires `admin`. It is
  enforced on the write path (the pre-receive hook, where old→new ancestry is
  known), not at request authorization — see [safety.md](safety.md).
- **The management UI reuses the same tokens.** A browser signs in by pasting a
  PAT once (verified through the identical path); the server then issues an
  HMAC-signed, stateless session cookie (no sessions table — no new source of
  truth). The UI is gated on the global-admin role (`users.is_admin`), distinct
  from per-repo ACLs.
- **SSH is deferred.** It's the expected forge UX and the `ssh_keys` schema is
  already there, but an SSH connection is stateful — awkward for a container that
  sleeps. Revisit once HTTP is solid.
