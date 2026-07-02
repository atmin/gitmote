# 08 — Bootstrap

Depends on: 07.

## Spec

Get from an empty bucket + empty s3lite to a usable instance: schema, a first
**admin** user + token, and a first repo — per
[notes/bootstrap.md](../docs/notes/bootstrap.md).

## Current

Schema is created on Open (task 03), but there is no entry path to create the
first admin/token/repo — a chicken-and-egg with token auth.

## Change

- `gitmote bootstrap` subcommand (arg-dispatched in `main`, no server): ensure
  schema, create an admin user, mint + print a token **once**, create an initial
  repo. Idempotent where safe; refuse to clobber an existing admin.
- Document the one-time flow in README / ops.

## Verify

- From a fresh MinIO bucket: run `bootstrap`, then use the printed token to
  `git push` and `git clone` the initial repo end-to-end.
- Re-running bootstrap neither duplicates nor overwrites the admin.
- Non-breaking: new subcommand; the server path is unchanged.
