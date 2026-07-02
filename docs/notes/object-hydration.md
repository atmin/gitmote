# Object hydration for reads

Getting the right git objects onto local disk for a given operation.

**Context.** The local bare repo is a cache; refs come from s3lite, objects from
S3 (see [ARCHITECTURE.md](../../ARCHITECTURE.md) → Components). The open point is
_how_ the objects arrive.

**The problem.** `upload-pack` (serving fetch/clone) and `receive-pack`'s
fast-forward / connectivity checks walk a _local_ object store. They do **not**
call back to fetch missing objects mid-operation — so "pull on demand" is not a
thing at the git layer. The needed closure must be present _before_ git runs.

## Options

1. **Full closure up front.** Before running git, hydrate the objects reachable
   from the refs involved (all refs for a clone; the target branch's history for
   a push). Simple, correct; cold-start cost ∝ repo size. Fine at the target
   scale (a handful of small repos).
2. **Warm cache.** Keep the materialized repo on disk while the container lives;
   after the first hydrate, only newly-referenced objects are fetched. Cuts
   repeat cold-start.
3. **Partial clone / promisor.** Make the local repo a promisor with S3 as the
   promisor remote plus a fetch helper, so git lazily pulls missing objects. Most
   scalable, most complex — the escape hatch when repo size becomes the wall.

## Leaning

(1) + (2): full reachable closure on cold start, kept warm for the container's
life. Revisit (3) only if large repos become real. This is the "scaling wall for
large repos" the architecture refers to.

## Open sub-points

- Computing the served closure for reads (reachable from advertised refs, capped
  by what the client may request).
- Eviction policy for warm repos under disk pressure.
- Whether a write needs the _full_ history or just enough for the
  fast-forward / merge-base check (bounded hydration).
