# 03 — Metadata layer (s3lite)

Depends on: 01.

## Spec

All mutable forge state — refs, repos, users, tokens, ACLs — in s3lite, per the
schema in [storage.md](../docs/architecture/storage.md). Refs are the source of
truth; a ref update is a single-transaction compare-and-swap
([safety.md](../docs/architecture/safety.md) §3). Durability (WAL → S3) is
s3lite's own concern.

## Current

Nothing yet. s3lite is a working dependency to import.

## Change

- Add `github.com/atmin/s3lite`; open it as `*sql.DB` inside a `Metadata` type.
- `internal/meta/schema.sql` + migrations run automatically on Open (idempotent)
  — the tables from storage.md: `repos`, `refs`, `users`, `tokens`, `ssh_keys`,
  `acls`.
- `internal/meta/` query layer: `ListRefs`, `CASRef(repo, name, old, new)` (one
  transaction; a multi-ref variant for atomic push), repo/user/token CRUD, ACL
  lookup. Tokens stored **hashed**, never raw.
- Test seed helpers (insert repo/user/token/refs directly) for later tasks.

## Verify

- Migrations create the schema on a fresh (empty) s3lite; re-open is a no-op.
- CAS unit tests: success on match; reject + rollback on mismatch; multi-ref
  is all-or-nothing.
- Non-breaking: DB layer + tests, no route yet.
