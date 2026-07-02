# Auth & transport

> Part of the [gitmote architecture](README.md).

- **Smart HTTP first.** `git http-backend` over HTTPS, authenticated with
  bearer **personal access tokens** (hashed in `tokens`). Stateless, firewall-
  friendly, and the natural fit for a scale-to-zero container.
- **Authorization is per-repo**, read from `acls` on every request.
- **The management UI reuses the same tokens.** A browser signs in by pasting a
  PAT once (verified through the identical path); the server then issues an
  HMAC-signed, stateless session cookie (no sessions table — no new source of
  truth). The UI is gated on the global-admin role (`users.is_admin`), distinct
  from per-repo ACLs.
- **SSH is deferred.** It's the expected forge UX and the `ssh_keys` schema is
  already there, but an SSH connection is stateful — awkward for a container that
  sleeps. Revisit once HTTP is solid.
