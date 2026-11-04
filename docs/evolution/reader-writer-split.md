# Reader/writer split with a leased writer

> **Status:** gitmote implements the writer half, the leased single writer below.
> s3lite `RoleAuto` provides it, and gitmote gates pushes on the lease. See
> [safety.md §1](../architecture/safety.md) and
> [configuration.md](../configuration.md#single-writer). An idle writer yields
> the lease after 5 s, so two long-lived instances take turns as the writer. The
> read-scaling half is speculative: `max-scale > 1`, with followers fresh enough
> to serve clones. Do not cite the read-scaling half as a spec.

## The problem

[safety.md §1](../architecture/safety.md) requires one writer at all times. Two
litestream writers on the same WAL can corrupt the metadata replica.

Without a lease, a deploy breaks this rule for a moment. Scaleway Serverless
Containers do rolling deploys: the new instance boots while the old one drains.
So the instance count rises to 2, as Cockpit shows.

One alternative is a stop-first self-drain. CI would call `POST /admin/quit`, and
the instance would send itself SIGTERM before the image swap. This closes the
window in practice, but not by proof. It cannot serve reads during the handoff,
and it leaves a re-wake race. The lease below solves the problem instead, so
gitmote has no drain step.

## The idea

A running instance is not always the writer. Many instances may run and serve
reads, and exactly one holds a writer lease.

## The writer half

- Exactly one instance holds the writer lease, and it is the litestream leader.
  It accepts `receive-pack`, does the ref CAS and replicates the WAL. A push to
  any other instance gets a retryable `503`.
- On a deploy, the new instance starts as a follower. The old instance releases
  the lease on SIGTERM, in `Close`. Then the new instance acquires the lease and
  promotes. Writes pause briefly. Promotion depends on the lease, so by
  construction two writers never exist.

### How it works

The coordination uses a conditional-write CAS on the replica's `lock.json`, with
`If-Match` and `If-None-Match`. The S3 provider must support these, and Scaleway
and AWS S3 do. This needs no second provider, and the WAL stays where it is.

litestream ships the lease primitive, `litestream.Leaser` and `s3.Leaser`:

- `If-None-Match:*` acquires the lease, and `If-Match` renews it.
- The TTL is 30 s.
- A `Generation` number tells contenders apart. A clean release deletes the lock,
  so the next holder starts again at 1. So `Generation` is no fencing token
  across a handoff.

litestream does not gate replication on the lease by itself. So s3lite wraps the
lease, in v0.1.0 and later. It offers `RoleAuto`, `RoleWriter` and `RoleFollower`,
with `IsLeader`, `OnPromote` and `OnDemote`. s3lite owns the leader lifecycle:

1. Acquire the lease.
2. Replicate the WAL.
3. Renew the lease at TTL/3.
4. Step down when the lease is lost.
5. Release the lease on `Close`.

gitmote consumes this. The server boots in `RoleAuto`, and `receive-pack` is
gated on `IsLeader()`. This is the shared coordination that
[safety.md §2](../architecture/safety.md) leaves room for: "in-process now,
shared coordination later".

The lease also settles a cold-start problem. A lone instance that wakes on a push
cannot tell two cases apart:

- It is the only instance, so it is safe to write.
- It is a new deploy, and the old instance keeps writing.

From inside, the two cases look the same. Only the external lease answers the
question "may I be the writer?".

## The read-scaling half

`max-scale` is 1. The lease makes more instances safe, and the idle yield lets
them take turns. But a follower serves no reads. gitmote gates every metadata
request on the lease, because a follower holds only the snapshot it restored. So
a follower could serve stale refs to a clone.

s3lite provides the freshness this needs. `FollowerRefreshInterval` catches a
follower up incrementally, in the background. `Refresh` does the same on demand,
so a follower could refresh just before it serves a read. gitmote uses neither.
The read path would also have to stop writing metadata. The auth guard stamps a
token's `last_used` on every request. The read-scaling
[open question](../architecture/open-questions.md) names this work.

Build it when reads get heavy enough to want replicas, or when `max-scale` must
exceed 1 for throughput. Until then, one leased writer at `max-scale = 1` is
enough, and simpler.
