# Components & storage

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
      │ lfs/      │
      └───────────┘
```

- **The container (Go).** Wraps `git http-backend` (the CGI program bundled with
  git that implements the entire smart-HTTP protocol — via Go's stdlib
  `net/http/cgi`), serves the web UI, and embeds s3lite as an `*sql.DB`.
- **S3.** Immutable git objects and packfiles.
- **s3lite.** Refs and all forge metadata. Source of truth for refs.
- **Ephemeral local disk.** A working bare repo per accessed repository — a
  **materialization / cache**, never the source of truth. Refs always come from
  s3lite; objects are hydrated from S3 into the closure an operation needs — a
  **write** hydrates the target branch's full history (fast-forward and
  connectivity checks demand it), a **read** hydrates the closure it must serve.
  On eviction or cold start the repo is simply rebuilt.

---

## Storage layout

### S3 (immutable — _content before pointer_)

Mirrors the on-disk bare repo's immutable directories, per repo prefix. Refs are
deliberately **excluded** — they live in s3lite.

| Prefix                                   | Contents                                         |
| ---------------------------------------- | ------------------------------------------------ |
| `{repo}/objects/…`                       | Loose git objects (git's own `ab/cdef…` fan-out) |
| `{repo}/objects/pack/pack-*.pack` `.idx` | Packfiles + indexes                              |
| `lfs/{repo}/{oid}`                       | Large-file blobs (deferred — see [open questions](open-questions.md)) |

Sync is done with the S3 SDK directly (single static binary, no external
dependency); `rclone` is a zero-code fallback. New objects/packs are PUT after
`receive-pack`; on fetch, missing objects are pulled on demand.

### s3lite schema (mutable — the reason s3lite is here)

The schema is defined **once**, in
[`internal/meta/schema.sql`](../../internal/meta/schema.sql) — the single source
of truth, applied idempotently (`CREATE ... IF NOT EXISTS`) on every `Open`, with
a guarded `ALTER` migration in
[`internal/meta/meta.go`](../../internal/meta/meta.go) for additive columns the
version-less store can't express in `schema.sql` alone. It is self-documenting
(every column is commented); this doc keeps only the cross-cutting rules the DDL
can't state. The tables, grouped: **`repos` / `refs`** (repos and their mutable
pointers — refs are the whole reason this DB exists); **`users` / `tokens` /
`ssh_keys` / `acls`** (identity and access); **`ci_runs` / `ci_jobs` /
`ci_secrets`** (CI); and **`server_secrets`** (auto-provisioned cookie/worker
keys). The rules that live here, not in the DDL:

`HEAD` is not a row — it derives from `repos.default_branch`. Every `refs` row
holds a concrete object id; symbolic refs beyond `HEAD` are not stored.

`users.is_admin` is the **global** administrator flag — the role that may create
users and repos and manage ACLs (the entry point is `gitmote bootstrap`, which
mints the first admin). It is distinct from `acls.perm='admin'`, which is
per-repo; day-to-day repo access is always governed by `acls`.

A PAT is a `selector.secret` pair. The **selector** is a non-secret, unique,
indexed lookup key; only the **verifier** — `SHA-256` of the **secret** half —
is stored. Verification looks the row up by selector, then compares the verifier
in **constant time**, so neither a timing side-channel on the lookup nor a
database leak yields a usable token. The raw `selector.secret` is shown to the
user exactly once at mint time and never persisted.
