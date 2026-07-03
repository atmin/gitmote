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

## Why it isn't shipped

The lease needs a coordination substrate with **atomic compare-and-swap**, which
is exactly what **Scaleway Object Storage lacks** (no `If-Match`/`If-None-Match`).
So it requires two real changes:

1. **Move the metadata WAL/lease to conditional-write storage** — Cloudflare R2 or
   AWS S3 support preconditions; Scaleway doesn't. Compute can stay on Scaleway.
2. **Leader election in s3lite** — acquire/refresh a lease (fencing token), and a
   litestream lifecycle that only replicates while leader. gitmote grows a
   reader/leader mode and promotion.

The cold-start subtlety that motivates the lease: a lone instance waking on a
push can't tell "I'm the only one, safe to write" from "I'm a new deploy while
the old still writes" — they look identical from inside. Only an external lease
answers "am I allowed to be the writer?".

## Trigger to build it

When any of: reads get heavy enough to want replicas; deploy-time write pauses or
the residual overlap window become unacceptable; or `max-scale` needs to exceed 1
for throughput. Until then the single-writer pin plus stop-first drain is
sufficient and far simpler.
