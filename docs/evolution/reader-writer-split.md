# Reader/writer split with a leased writer

> **Status: writer half shipped; read-replica half still speculative.** The
> **leased single writer** below is now implemented (s3lite `RoleAuto`; gitmote
> gates pushes on the lease — see [safety.md §1](../architecture/safety.md) and
> [ops.md](../ops.md)). What remains speculative is the **read-scaling** half:
> `max-scale > 1` with followers fresh enough to serve clones. Don't cite the
> read-replica part as a spec.

## The problem it solved

[safety.md §1](../architecture/safety.md) requires **one writer, ever** — two
litestream writers on the same WAL can corrupt the metadata replica.

A **deploy** momentarily broke it. Scaleway Serverless Containers do rolling
deploys — the new instance boots while the old drains — so instance count spikes
to 2 (observed in Cockpit). The original interim was a **stop-first self-drain**
(CI `POST /admin/quit` → self-SIGTERM before the image swap): it closed the window
_in practice_ but not _by proof_, couldn't serve reads during handoff, and left a
re-wake race. **This is now resolved by the lease** (below) — the drain is gone.

## The idea (writer half — shipped)

Stop conflating "an instance is running" with "an instance is the writer." Let
**many instances run and serve reads**; let **exactly one hold a writer lease**.

- Exactly one instance holds a **writer lease** and is the litestream leader: it
  accepts `receive-pack`, does the ref CAS, and replicates the WAL. A push to a
  non-leader is rejected (retryable `503`).
- **Handoff on deploy:** the new instance comes up as a follower; when the old
  releases the lease (on SIGTERM `Close`), the new acquires it and promotes. A
  brief write pause, and — because promotion is gated on the lease — _never_ two
  writers, by construction.

**How it shipped.** The coordination substrate is a conditional-write CAS on the
replica's `lock.json` (`If-Match`/`If-None-Match`, which the S3 provider must support —
Scaleway and AWS S3 do) — no second provider, no moving the WAL. litestream ships the lease primitive
(`litestream.Leaser` / `s3.Leaser`: `If-None-Match:*` to acquire, `If-Match` to
renew, 30 s TTL, monotonic `Generation` fencing token), but does **not** auto-gate
replication on it. So **s3lite** wraps it (v0.1.0, `RoleAuto`/`RoleWriter`/
`RoleFollower`, `IsLeader`/`OnPromote`/`OnDemote`), owning the leader lifecycle
(acquire → replicate → renew at TTL/3 → step down on loss → release on `Close`),
and **gitmote** consumes it: server boots `RoleAuto`, receive-pack is gated on
`IsLeader()`. This is the split [safety.md §2](../architecture/safety.md)
anticipated ("in-process now, shared coordination later").

The cold-start subtlety the lease resolves: a lone instance waking on a push can't
tell "I'm the only one, safe to write" from "I'm a new deploy while the old still
writes" — identical from inside. Only the external lease answers "am I allowed to
be the writer?".

## The read-scaling half — still open

`max-scale` stays **1**. The lease makes running more instances _safe_, but not
yet _useful_ for reads: an s3lite follower serves a **restored snapshot and
polls** — it is not a continuously-fresh replica, so it could serve stale refs to
a clone. Scaling reads out needs follower freshness first (litestream `restore -f`
follow mode, or VFS read replicas + `PRAGMA litestream_write_enabled` — the latter
likely needs cgo, incompatible with s3lite's pure-Go `modernc` driver; verify).
This is what the read-scaling [open question](../architecture/open-questions.md)
names.

**Trigger to build it:** reads get heavy enough to want replicas, or `max-scale`
needs to exceed 1 for throughput. Until then, one leased writer at `max-scale = 1`
is sufficient and simpler.
