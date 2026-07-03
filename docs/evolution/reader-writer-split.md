# Reader/writer split with a leased writer (speculative)

> **Status: evolution note — speculative, not a commitment.** Where gitmote could
> go, captured so the reasoning survives a fresh session. Nothing here is planned
> or specced; don't cite it as a spec.

## The problem it solves

[safety.md §1](../architecture/safety.md) requires **one writer, ever** — two
litestream writers on the same WAL can corrupt the metadata replica. Today that's
enforced by running a single container (`max-scale = 1`).

But a **deploy** momentarily breaks it. Scaleway Serverless Containers do rolling
deploys — the new instance boots while the old drains — so instance count spikes
to 2 (observed in Cockpit), i.e. two writers. Scaleway won't let you scale to 0
(`max-scale` min is 1), so it can't stop-first on its own.

The shipped interim ([ops.md](../ops.md) — deploy) is **stop-first via self-drain**:
CI `POST /admin/quit`s the running writer (self-SIGTERM → graceful flush + exit)
before swapping the image, so the swap starts a fresh instance with no old one to
overlap. That closes the window *in practice* but not *by proof* — a request
racing the ~20 s drain→swap window can re-wake the old image. It also can't serve
reads during the handoff (brief), and cold-start-from-zero must just-write.

## The idea

Stop conflating "an instance is running" with "an instance is the writer." Let
**many instances run and serve reads**; let **exactly one hold a writer lease**.

- Every instance boots as a **read replica**: serves `upload-pack` (clone/fetch),
  UI reads, `/healthz`. It restores the metadata DB from the replica and follows
  it read-only; it does **not** replicate or accept pushes.
- Exactly one instance holds a **writer lease** and is the litestream leader: it
  accepts `receive-pack`, does the ref CAS, and replicates the WAL. A push to a
  non-leader is rejected/redirected.
- **Handoff on deploy:** the new instance comes up as a reader; when the old
  releases the lease (on SIGTERM drain), the new acquires it and promotes. Zero
  read downtime, a brief write pause, and — because promotion is gated on the
  lease — *never* two writers, by construction.

This is the split [safety.md §2](../architecture/safety.md) already anticipates
("in-process now, shared coordination later… the SQL CAS keeps the door open")
and the "litestream read replicas" the read-scaling
[open question](../architecture/open-questions.md) names.

## What it needs — now unblocked

The lease needs a coordination substrate with **atomic compare-and-swap**. When
this note was written, Scaleway Object Storage lacked it — but **Scaleway now
supports conditional writes** (`If-Match` / `If-None-Match`), so the lease can
live on the same Scaleway bucket; no second provider, no moving the WAL.

Better still, **litestream already ships the lease primitive** (v0.5.8+,
`litestream.Leaser` / `s3.Leaser`): `If-None-Match: *` to acquire, `If-Match` to
renew, a 30 s TTL and a monotonic `Generation` (fencing token). We do **not**
hand-roll CAS/fencing. The catch: litestream does **not** auto-gate its own
replication on the lease (neither `Store` nor `DB` checks it, as of v0.5.13) — so
the leader lifecycle (acquire → replicate → renew → step down on loss → release)
is orchestration s3lite must add around litestream's primitive.

The cold-start subtlety that motivates the lease: a lone instance waking on a
push can't tell "I'm the only one, safe to write" from "I'm a new deploy while
the old still writes" — they look identical from inside. Only an external lease
answers "am I allowed to be the writer?".

This is now a concrete piece of work, specced in s3lite (`tasks/single-writer-lease.md`
in that repo), since the primitive lives there. gitmote then consumes it: boot as
reader, promote on lease acquisition, reject pushes when not leader.

## Trigger to build it

When any of: reads get heavy enough to want replicas; deploy-time write pauses or
the residual overlap window become unacceptable; or `max-scale` needs to exceed 1
for throughput. Until then the single-writer pin plus stop-first drain is
sufficient and far simpler — the value now is that the substrate is available, so
it's build-when-ready rather than blocked.
