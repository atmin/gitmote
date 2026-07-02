# Request flows

> Part of the [gitmote architecture](README.md).

## Clone / fetch (read path)

No lock. Refs come from s3lite; the object closure to serve is hydrated from S3.

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

## Push (write path) — the CAS

Serialized by a per-repo in-process mutex; the safety-critical ordering is
**objects durable in S3 first, ref CAS in s3lite second.** The catch that shapes
everything below: `git receive-pack` updates refs and acknowledges the client
_itself_, at the end of its run — so the durable commit cannot happen _after_ it
returns (the ack has already gone out). It must gate _inside_ receive-pack's
lifecycle, at its one designed seam: the `pre-receive` hook.

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

**Why it looks like this:**

- **`pre-receive` is the transaction boundary.** It fires with every
  `<old> <new> <ref>` command before any ref changes, while the pushed objects
  sit in a quarantine dir (`$GIT_QUARANTINE_PATH`). Exit non-zero and git rejects
  the whole push and throws the quarantine away — so a failed S3 PUT or CAS
  leaves nothing behind. Quarantine also isolates _exactly_ the new objects, so
  "PUT the new objects" is simply "PUT the quarantine contents."
- **The hook can't touch s3lite directly.** It runs as a child of
  `receive-pack` — a _separate process_ — and s3lite is single-writer SQLite
  embedded in the parent. Two processes writing it is the one thing s3lite
  forbids. So the hook RPCs back to the parent over a unix socket; the parent,
  the sole writer, performs the PUT + CAS and returns a verdict. (This is exactly
  how GitLab/Gitaly wires its git hooks back to the app.)
- **One SQL transaction = atomic multi-ref push.** All per-ref CAS run inside a
  single s3lite transaction; all-or-nothing matches `git push --atomic`, and is
  _stronger_ than loose-file refs.
- **The local refs are a throwaway.** On an `ok` verdict, receive-pack migrates
  the quarantine and updates on-disk refs — bookkeeping on disposable disk;
  durability is S3 + s3lite. Note the fast-forward / ancestry check needs the
  target branch's history present locally, which is why a _write_ hydrates that
  history up front (and is the scaling wall for large repos).
