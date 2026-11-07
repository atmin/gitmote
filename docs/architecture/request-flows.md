# Request flows

This file is part of the [gitmote architecture](README.md). It shows the read
path and the write path of a git request.

## Clone and fetch: the read path

A read takes no lock. The refs come from s3lite. gitmote hydrates the object
closure to serve from S3.

```mermaid
sequenceDiagram
    participant C as git client
    participant G as gitmote
    participant D as s3lite
    participant S as S3
    C->>G: GET /{repo}/info/refs?service=git-upload-pack
    G->>D: authorize (read on repo)
    G->>D: SELECT name, sha FROM refs WHERE repo_id=?
    G->>G: materialize refs into working repo
    G-->>C: advertise refs
    C->>G: POST /{repo}/git-upload-pack (wants/haves)
    G->>S: hydrate the object closure to serve
    G->>G: git upload-pack (negotiate + build packfile)
    G-->>C: packfile
```

## Push: the write path and the CAS

A per-repo mutex in the process serializes pushes. The order of two steps is
critical for safety:

1. The objects become durable in S3.
2. Then the ref compare-and-swap (CAS) runs in s3lite.

One fact shapes the whole design. `git receive-pack` itself updates the refs and
acknowledges the client, at the end of its run. So the durable commit cannot
happen after receive-pack returns, because the acknowledgement has already gone
out.

The commit must happen inside the run of receive-pack. receive-pack has one hook
point designed for this: the `pre-receive` hook.

```mermaid
sequenceDiagram
    participant C as git client
    participant R as receive-pack + hook
    participant G as gitmote (sole writer)
    participant S as S3
    participant D as s3lite
    C->>G: POST /{repo}/git-receive-pack (packfile + ref commands)
    G->>D: authorize (write on repo)
    G->>G: acquire per-repo write lock
    G->>D: read ref tips → materialize working repo
    G->>R: spawn receive-pack, pipe request body
    R->>R: index pack to quarantine · enforce fast-forward
    R->>G: pre-receive RPC (ref commands + quarantine path)
    G->>S: PUT quarantined objects — content before pointer
    G->>D: BEGIN · per-ref CAS (WHERE sha=:expected) · COMMIT
    Note over G,D: any mismatch ⇒ ROLLBACK ⇒ reject
    G-->>R: verdict (ok / reject)
    R->>R: ok ⇒ migrate quarantine + update local refs · else ⇒ discard
    R-->>G: report-status
    G-->>C: response (per-ref status)
    G->>G: release lock
```

## Why the push works this way

### `pre-receive` is the transaction boundary

The hook runs with every `<old> <new> <ref>` command, before any ref changes. At
that point, the pushed objects sit in a quarantine directory
(`$GIT_QUARANTINE_PATH`).

When the hook exits non-zero, git rejects the whole push and discards the
quarantine. So a failed S3 PUT or CAS leaves nothing behind in the repo on disk.
At worst, orphan objects stay in S3.

The quarantine also holds exactly the new objects. So to PUT the new objects,
gitmote PUTs the contents of the quarantine.

### The hook calls back to the parent

The hook cannot touch s3lite directly. It runs as a child of `receive-pack`, in a
separate process. s3lite is a single-writer SQLite, embedded in the parent. The
one thing s3lite forbids is two processes that write to it.

So the hook calls the parent back over a unix socket. The parent is the sole
writer. It runs the PUT and the CAS, and returns a verdict. GitLab, through
Gitaly, connects its git hooks back to the app in the same way.

The channel has three parts:

- Installation. One hook binary, `gitmote-hook`, serves every repo.
  `core.hooksPath` refers to it. gitmote sets that value with `GIT_CONFIG_*` for
  each `receive-pack` it spawns. So every materialized repo has the hook, and no
  repo needs its own install.
- Discovery and auth. The parent puts the socket path (`GITMOTE_SOCK`) and a
  single-use nonce (`GITMOTE_NONCE`) into the environment of `receive-pack`. The
  parent mints the nonce for each push, and binds it to the locked operation. The
  nonce works once: its first use deletes it. So a stray local process cannot
  forge a ref update.
- Fail closed. Any error on the hook side rejects the push. Examples are:
  - a missing env var
  - a failed dial
  - a bad verdict

### One SQL transaction makes a push of many refs atomic

All the per-ref CAS checks run inside one s3lite transaction. So the push
succeeds or fails as a whole, as with `git push --atomic`. This is stronger than
refs stored as loose files.

### The local refs are disposable

On an `ok` verdict, receive-pack moves the quarantine into the repo and updates
the refs on disk. This is only bookkeeping on a disposable disk. S3 and s3lite
hold the durable state.

The fast-forward check tests ancestry, so it needs the history of the target
branch on the local disk. So a write hydrates that history first. This is the
limit on scale for large repos.
