# Open questions

> Part of the [gitmote architecture](README.md).

An item with worked-out context has its own note in [`../notes/`](../notes/),
one concern to a note. The other items stay short here until they need a note.

## Object hydration for reads

`upload-pack` walks a local object store. So the object closure must be on disk
before it runs, because git cannot fetch objects on demand. The choice is between
a full hydrate and a partial clone with a promisor remote. See
[../notes/object-hydration.md](../notes/object-hydration.md).

## gc and compaction

Loose objects pile up. A `cleanup` subcommand, triggered by the loose-object
count, would repack and prune orphans. Pruning must not race in-flight reads. A
fetch pins a ref snapshot from s3lite, and a push only adds objects. So reads are
safe against pushes, but not against deletion. The sweep needs a grace window or
a liveness check.

## Git LFS

Large blobs would upload with a presigned PUT to `lfs/{repo}/{oid}`.
[storage.md](storage.md) reserves this key in the storage layout.

## Web-authored commits

How does the service build a commit from a web edit? There are two options:

1. Build the tree and commit objects, and drive them through the same path as a
   push: `receive-pack`, then the CAS.
2. Take a shortcut: write the objects directly, then do the CAS.

## Closing the litestream window for refs

Do this only if the accepted loss of an acknowledgment proves unacceptable. See
[safety.md](safety.md) §4.

## Read scaling

One container serves both reads and writes. If reads get heavy, litestream read
replicas are the answer.
