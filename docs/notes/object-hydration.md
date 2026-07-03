# Object hydration for reads

Getting the right git objects onto local disk for a given operation.

**Context.** The local bare repo is a cache; refs come from s3lite, objects from
S3 (see [architecture → components](../architecture/storage.md)). The open point is
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

## Progress

- **Advertisement needs no objects (being done — `tasks/13`).** The `info/refs`
  GET only lists refs, which live in s3lite; it can serve from `packed-refs` with
  zero object hydration (spiked: `git upload-pack --advertise-refs` advertises
  refs whose objects are absent, branches and annotated tags alike). This carves
  the most common cold round-trip off the full-hydrate path first.
- **Bounded closure for the data POSTs is deferred.** The sharp part: computing
  "objects reachable from ref X" without the objects present needs a precomputed
  **reachability + pack-location index** (built at push, consumed at hydrate),
  because stock git can't fetch mid-operation and packs are all-or-nothing. That
  index is the real design work; parked until it's understood well enough to spec.

## Open sub-points

- The reachability+location index for bounded POST hydration: shape (commit graph
  + object→pack map in s3lite), how it's kept consistent with content-before-
  pointer, and its interaction with any future server-side `gc`/repack.
- Eviction policy for warm repos under disk pressure.
- Whether a write needs the _full_ history or just enough for the
  fast-forward / merge-base check (bounded hydration).
