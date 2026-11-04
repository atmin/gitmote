# Object hydration for reads

This note covers how the git objects that an operation needs reach local disk.

## Context

The local bare repo is a cache. Refs come from s3lite, and objects come from S3
(see [the components](../architecture/storage.md) of the architecture). The open
point is how the objects arrive.

## Problem

Two parts of git walk a local object store:

- `upload-pack`, which serves fetch and clone
- the fast-forward and connectivity checks of `receive-pack`

Neither one calls back to fetch a missing object during the operation. So git
cannot pull objects on demand. The closure that an operation needs must be on
disk before git runs.

## Options

1. **Full closure up front.** Before git runs, hydrate the objects reachable from
   the refs involved. For a clone, that is all refs. For a push, it is the
   history of the target branch. This option is simple and correct. Its
   cold-start cost grows with repo size. That is fine at the target scale of a
   handful of small repos.
2. **Warm cache.** Keep the materialized repo on disk while the container lives.
   After the first hydrate, gitmote fetches only newly referenced objects. This
   cuts the cost of repeat cold starts.
3. **Partial clone with a promisor.** Make the local repo a promisor, with S3 as
   the promisor remote and a fetch helper. Then git pulls missing objects lazily.
   This option scales best, and it is the most complex. It is the escape hatch
   for when repo size becomes the limit.

## Preferred options

The preferred options are 1 and 2. A cold start hydrates the full reachable
closure, and the closure stays warm while the container lives. Revisit option 3
only if large repos become a real need. Hydrating the full closure up front is
the limit on scale for large repos that the architecture mentions.

## Current state

The ref advertisement needs no objects. The `info/refs` GET only lists refs,
which live in s3lite. So it serves them from `packed-refs`, with zero object
hydration. A spike confirmed that `git upload-pack --advertise-refs` advertises
refs whose objects are absent. This holds for branches and annotated tags alike.
So the most common cold round trip skips the full hydrate.

The data POSTs use the full hydrate, and a bounded closure for them is deferred.
The hard part is to compute the objects reachable from a ref without those
objects on disk. That needs a precomputed index of reachability and pack
location. A push builds the index, and a hydrate reads it. Stock git cannot fetch
during an operation, and a pack is all or nothing, so the index is necessary.
This index is the real design work. It waits until it is understood well enough
to specify.

## Open sub-points

- The reachability and location index for a bounded POST hydrate:
  - its shape: a commit graph and an object-to-pack map in s3lite
  - how it stays consistent with content before pointer
  - how it interacts with a future server-side `gc` or repack
- The eviction policy for warm repos under disk pressure.
- Whether a write needs the full history, or just enough for the fast-forward
  and merge-base check (a bounded hydrate).
