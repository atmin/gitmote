# Components and storage

> Part of the [gitmote architecture](README.md).

## Components

```
┌──────────────────────────────────────────────────────────────┐
│  gitmote container (Go, single writer)                       │
│                                                              │
│   HTTP  ─┬─ smart-HTTP handler ── spawns ── git http-backend │
│          │                                    (stock git)    │
│          ├─ web UI (dashboard, repos, tokens, ACLs, CI)      │
│          └─ embedded s3lite (*sql.DB)                        │
│                                                              │
│   ephemeral disk: working bare repos (a CACHE, disposable)   │
└───────────┬─────────────────────────────────┬────────────────┘
            │ objects / packs                 │ WAL replication (litestream)
            ▼                                 ▼
      ┌───────────┐                     ┌───────────┐
      │    S3     │                     │  S3 (WAL) │  ← s3lite's backup target
      │ objects/  │                     └───────────┘
      │ ci/ logs  │
      └───────────┘
```

gitmote has four components:

- **The container (Go).** It wraps `git http-backend` through `net/http/cgi` from
  Go's standard library. It also serves the web UI, and embeds s3lite as an
  `*sql.DB`.
- **S3.** It holds the immutable git objects and packfiles.
- **s3lite.** It holds the refs and all forge metadata. It is the source of truth
  for refs.
- **Ephemeral local disk.** It holds a working bare repo for each repository that
  a request accesses.

`git http-backend` is the CGI program that ships with git. It implements the whole
smart-HTTP protocol.

### The local repo is a cache

Each working bare repo is a materialization, or cache, and never the source of
truth. Refs always come from s3lite. Every materialization makes the refs on disk
mirror s3lite exactly. It writes `packed-refs` from the metadata DB, and removes
the loose refs. A cache can go stale while another instance holds the lease. Even
so, it never advertises a ref that moved or was deleted.

Objects come from S3. The `info/refs` advertisement needs no objects, so it
hydrates none. The data requests of a fetch or a push copy every object of the
repo that the disk lacks. A write needs the target branch's full history, for its
fast-forward and connectivity checks. On eviction or a cold start, gitmote
rebuilds the repo.

## Storage layout

### S3: immutable, content before pointer

The S3 layout mirrors the immutable directories of the bare repo on disk. It lives
under the storage root: the whole bucket, or a `bucket/base` sub-path (see
[configuration.md](../configuration.md#storage-root)). Refs are deliberately
excluded. They live in s3lite, which is also under the root, at `meta/`. So
objects and metadata share fate. The keys below are relative to the root.

| Key | Contents |
| --- | --- |
| `{repo}/objects/…` | Loose git objects, in git's own `ab/cdef…` fan-out |
| `{repo}/objects/pack/pack-*.pack` `.idx` | Packfiles and their indexes |
| `ci/{repo}/{run}/{job}.log` | CI run logs, append-only |

gitmote syncs with the S3 SDK directly. So it stays a single static binary, with
no external dependency. `rclone` is a fallback that needs no code. New objects and
packs are PUT after `receive-pack` receives them. On a fetch, gitmote pulls the
missing objects for that request.

### s3lite schema: mutable, the reason s3lite is here

The schema is defined once, in
[`internal/meta/schema.sql`](../../internal/meta/schema.sql). That file is the
single source of truth. Every `Open` applies it idempotently, with
`CREATE ... IF NOT EXISTS`. The store has no schema version, so `schema.sql` alone
cannot add a column to an existing table. A guarded `ALTER` migration in
[`internal/meta/meta.go`](../../internal/meta/meta.go) adds such additive columns.

The schema documents itself, because every column has a comment. This doc keeps
only the rules that span tables and that the DDL cannot state. The tables fall
into four groups:

| Group | Tables |
| --- | --- |
| Repos and their mutable pointers. Refs are the whole reason this DB exists. | `repos`, `refs` |
| Identity and access | `users`, `tokens`, `ssh_keys`, `acls` |
| CI | `ci_runs`, `ci_jobs`, `ci_secrets` |
| The auto-provisioned cookie and worker keys | `server_secrets` |

The rules below live here, not in the DDL.

`HEAD` is not a row. It derives from `repos.default_branch`. Every `refs` row holds
a concrete object id. Symbolic refs other than `HEAD` are not stored.

`users.is_admin` is the global administrator flag. This role may create users and
repos, and manage ACLs. `gitmote bootstrap` mints the first admin. The server
runs the same bootstrap on an empty storage root. The flag is distinct from
`acls.perm='admin'`, which is per repo. The `acls` table always governs day-to-day
repo access.

A PAT is a `selector.secret` pair. The selector is a lookup key that is not
secret, unique and indexed. Only the verifier is stored: the `SHA-256` of the
secret half. Verification looks the row up by selector, then compares the verifier
in constant time. So neither a timing side channel on the lookup nor a database
leak yields a usable token. The user sees the raw `selector.secret` exactly once,
at mint time, and gitmote never persists it.
