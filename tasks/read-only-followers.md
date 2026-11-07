# Read-only followers: serve reads without the lease

## Spec

A follower serves reads while another instance holds the writer lease. Only
writes need the lease. A read on a follower first catches up to the replica, so
it lags the writer by at most the litestream window of under a second
([safety.md §4](../docs/architecture/safety.md)). The single-writer rule of
[safety.md §1](../docs/architecture/safety.md) stays as it is.

A read is one of these requests:

- A `git-upload-pack` request: the `info/refs` advertisement and the POST.
- A UI `GET` page.

A write is one of these requests:

- A `git-receive-pack` request: the `info/refs?service=git-receive-pack` GET and
  the POST.
- A UI `POST`.
- Any request under `/internal/ci/`. The claim is a `GET`, and it writes.

## Current

- `leaderGate` in `cmd/gitmote/main.go` gates every metadata request on the
  lease. A follower calls `TryPromote`, and returns a retryable `503` while
  another instance holds the lease.
- So a read takes the lease. It then blocks the other instance's pushes until
  this instance has been idle for 5 s (`idleYieldAfter`).
- A follower never catches up before promotion. gitmote calls neither
  `s3lite.Refresh` nor sets `FollowerRefreshInterval`.
- `Guard.verify` calls `meta.TouchToken` on every authenticated request. The
  `UPDATE` would fail on a follower's read-only database. No follower reaches
  the guard today, because the gate refuses first.
- `githttp` already refuses `git-receive-pack` with a `503` unless `IsWritable`
  is true. It serves `git-upload-pack` in either role.

## Change

1. **Classify each request in the gate.** `leaderGate` tells reads from writes by
   the rules in the Spec. A write keeps today's path: `TryPromote`, else `503`.
2. **Refresh, then serve, a read on a follower.** When `IsLeader` is false, the
   read calls a new `meta.Metadata.Refresh`, bounded by `promoteTimeout`, and
   then serves. It wraps `s3lite.DB.Refresh`. A read never promotes. On the writer, `Refresh` is a no-op that makes
   no network call. When `Refresh` fails, the read gets the `503` with
   `Retry-After`, as today.
3. **Stop the read path from writing.** `meta.TouchToken` returns `nil` without a
   write when the instance is not the writer. `last_used` then records only the
   uses that reached the writer. Confirm that `IsLeader` is true for an
   unreplicated sole writer, so local dev still stamps.
4. **Check the UI for writes on reads.** Confirm that no UI `GET` writes metadata.
   Confirm that `POST /login` and `POST /logout` write nothing. If both are true,
   classify those two as reads, so a user can sign in on a follower.
5. **Docs.** Update `safety.md` §1, the single-writer section of
   `configuration.md`, `evolution/reader-writer-split.md` and the read-scaling
   entry in `open-questions.md`. Update the comments on `leaderGate` and `gate`.

Trade-offs:

- Every read on a follower costs at least one S3 request, for the `Refresh`
  probe. A periodic `FollowerRefreshInterval` would be cheaper, but staler.
- Read-your-writes across instances is not guaranteed. A push to A, then an
  immediate fetch from B, can miss the commit for under a second.
- A follower's UI shows no live log of a running CI job. The live log lives in
  the writer's memory. The stored log appears once the job completes.
- A write still gets the `503` while the other instance is busy.
- `max-scale` stays 1. Routing pushes to the writer behind one URL stays out of
  scope.

## Verify

- `cmd/gitmote` `TestLeaderGate`: a follower serves a read after `Refresh`. A
  failed `Refresh` gives `503`. A write with the lease held elsewhere gives
  `503`, and a write promotes when the lease is free. The probes stay up.
- `cmd/gitmote`: the classifier marks the `git-receive-pack` advertisement, the
  push POST, UI POSTs and the `/internal/ci/` claim `GET` as writes. It marks the
  `git-upload-pack` advertisement, the fetch POST and UI `GET` pages as reads.
- `internal/meta`: `TouchToken` on a follower returns `nil` and leaves
  `last_used` unchanged. On the writer, it stamps `last_used`.
- `make e2e-handoff` gains a step. A receives a push. Within A's 5 s idle window,
  B serves a clone that contains the push, while `meta/lock.json` still exists. A
  push to B in that window gets the `503`.
- `make all`, `make e2e-local` and `make e2e-restore` stay green.
