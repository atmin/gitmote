# Idle lease handoff — two live instances on one storage root

## Spec

Two long-lived instances on the same storage root both serve — for example an
always-on local container and the cloud instance. Whichever instance gets a
request takes the writer lease, and no instance pins the lease while it is idle.
The lease still enforces the single-writer invariant
([safety.md §1](../docs/architecture/safety.md)), and a handoff never serves a
stale ref.

## Current

- The server opens its metadata in s3lite `RoleAuto` (s3lite v0.4.0). The holder
  renews the lease every 10 s for as long as it lives.
- `leaderGate` serves a metadata request only on the writer, and promotes on
  demand through `TryPromote`. So a second live instance answers `503` to every
  request until the first one exits.
- The materialized cache writes refs from the metadata layer, but never removes
  a loose ref. `MaterializeRefs` rewrites only `packed-refs`, and loose refs
  override it. With one instance this is harmless: its own `receive-pack` makes
  every ref change. After a handoff, the cache would advertise a ref that the
  other instance moved or deleted.

## Change

1. **Bump s3lite to v0.14.1.** v0.9.0 added `YieldLease` and
   `OnDemandPromotion`. s3lite carries a litestream fork through a `replace`, and
   Go ignores a dependency's `replace`, so gitmote's `go.mod` repeats it. The S3
   leaser is byte-identical in litestream v0.5.13 and the fork, so the lock of a
   rolling deploy from the old build stays compatible.
2. **Promote only on demand.** `meta.Config` gains `OnDemandPromotion`, and the
   server sets it whenever a replica is configured. `meta.Metadata` wraps
   `YieldLease`. A yielded instance then never takes the lease back in the
   background; the request gate takes it when a request needs it.
3. **Yield after 5 s idle.** A `leaseActivity` wraps the handler. Every request
   except the probes and static assets holds its read lock for its whole life,
   and stamps its end time. Once a second a loop yields the lease when the
   instance is the writer, has been idle for 5 s, and gets the write lock without
   waiting — so no request is in flight. A request that arrives during the yield
   waits for the lock, and the gate then promotes again. The CI reconcile tick
   runs under the read lock, so a sweep never races the yield.
4. **The cache mirrors the metadata refs exactly.** `Materialize` and
   `MaterializeRefs` write `packed-refs` from the metadata layer, then remove the
   loose refs. The refs on disk then equal the database's, moved and deleted refs
   included.
5. **Docs.** The README, `ops.md` (the single-writer section and bootstrap),
   `safety.md` §1, `storage.md` (the cache) and `evolution/reader-writer-split.md`
   describe the handoff.

Trade-offs:

- A request to one instance while the other is busy still gets `503`, until the
  other has been idle for 5 s. git reports it as a failed fetch or push.
- The first request after an idle period pays a lease acquisition. It resumes in
  place when no other instance wrote, and restores when one did.
- Reads without the lease (followers that `Refresh` and serve) stay out of scope.

## Verify

- `cmd/gitmote`: the loop yields when the instance is an idle writer. It does not
  yield while a request is in flight, before 5 s, or as a follower. The probes do
  not count as activity. A request that arrives during a yield waits, then runs.
- `cmd/gitmote`: the config sets `OnDemandPromotion` only with a replica.
- `internal/repo`: a moved and a deleted ref, stale on disk, match the metadata
  layer after `MaterializeRefs` and after `Materialize`.
- `make e2e-handoff`, also in CI: two containers on one MinIO root. B clones what
  A received. B moves one branch and deletes another, and A then advertises
  exactly B's refs. The lease file is gone once both are idle.
- `make all`, `make e2e-local` and `make e2e-restore` stay green.
