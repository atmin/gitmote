# 06 — Auth: PATs + per-repo ACLs

Depends on: 03, 05.

## Spec

Authenticate smart-HTTP with bearer **personal access tokens** (hashed in
`tokens`) and authorize **per-repo** from `acls` on every request, per
[auth.md](../docs/architecture/auth.md).

## Current

The read path (task 05) is open to anyone.

## Change

- `internal/auth` — PAT mint (return the raw token once, store only its hash) +
  verify with constant-time compare; update `last_used`.
- A request guard resolving `Authorization: Bearer …` → user, and an
  `authorize(repo, user, perm)` helper reading `acls`.
- Apply `read` authorization to the task-05 routes. `401` for missing/invalid
  token, `403` for insufficient permission, using git's expected auth challenge
  so `git` and credential helpers behave.

## Verify

- Golden: clone with a valid `read` token succeeds.
- Rejections (not optional): no token → 401; unknown/bad token → 401; valid
  token lacking `read` on the repo → 403.
- Non-breaking: the read path now requires auth by design; read tests are
  updated to seed a token + ACL. No prior external consumers exist to break
  (design stage).
