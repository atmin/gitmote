# Bootstrap

Getting from an empty bucket + empty s3lite to a usable instance.

**Context.** Nothing works until a user, a token, and a repo exist — and the
s3lite schema itself must be created. None of this has an entry path yet.

## What must exist on day one

- The s3lite schema (tables from [ARCHITECTURE.md](../../ARCHITECTURE.md) →
  Storage layout). Run migrations automatically on s3lite `Open`.
- A first **admin** user + a token to authenticate as them.
- At least one repo to push to.

## Options

1. **Env-seeded admin.** On first boot, if `users` is empty, seed an admin from
   `GITMOTE_ADMIN_HANDLE` + a token hash in env. Zero-interaction — good for a
   scale-to-zero container.
2. **CLI subcommand.** `gitmote admin add-user`, `gitmote repo create`, … —
   arg-based, no server, run inside the one writer container (mirrors s3lite's
   single-writer rule). Good for ongoing admin.
3. **Web setup wizard** on first run. More UX, more code.

## Leaning

(1) for the very first admin (idempotent seed), (2) for everything after. Schema
migrations run on `Open`.

## Open sub-points

- **Schema gap:** `acls.perm='admin'` is _per-repo_. A **global** admin (who may
  create users/repos) isn't modeled — likely needs a `users.is_admin` flag or a
  global-role table. Resolve before building bootstrap.
- Token format + hashing (argon2id vs sha256; the raw token is shown once).
- Repo-creation rules — the `owner/name` convention (`repos.name`), who may
  create, reserved names.
