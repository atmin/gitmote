# Auth & transport

> Part of the [gitmote architecture](README.md).

- **Smart HTTP first.** `git http-backend` over HTTPS, authenticated with
  bearer **personal access tokens** (hashed in `tokens`). Stateless, firewall-
  friendly, and the natural fit for a scale-to-zero container.
- **Authorization is per-repo**, read from `acls` on every request.
- **SSH is deferred.** It's the expected forge UX and the `ssh_keys` schema is
  already there, but an SSH connection is stateful — awkward for a container that
  sleeps. Revisit once HTTP is solid.
