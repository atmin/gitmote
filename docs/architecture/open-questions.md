# Open questions

> Part of the [gitmote architecture](README.md).

Items with worked-out context live as one-concern notes under
[`../notes/`](../notes/); the rest stay one-liners until they earn a note.

- **Object hydration for reads.** `upload-pack` walks a _local_ object store, so
  the closure must be present before it runs ("on demand" doesn't apply at the
  git layer); full-hydrate vs partial-clone/promisor —
  [../notes/object-hydration.md](../notes/object-hydration.md).
- **Bootstrap.** Creating the first admin, token, and repo from an empty bucket —
  [../notes/bootstrap.md](../notes/bootstrap.md).
- **gc / compaction.** Loose objects proliferate; a `cleanup` subcommand
  (triggered by loose-object count) repacks and prunes orphans. Pruning must not
  race in-flight reads — a fetch pins a ref snapshot from s3lite and pushes only
  _add_ objects, so reads are safe against pushes, but _deletion_ is not; the
  sweep needs a grace window or liveness check.
- **Git-LFS.** Large blobs via presigned PUT to `lfs/{repo}/{oid}` — schema slot
  reserved in [storage.md](storage.md).
- **Web-authored commits.** How the service constructs a commit from a web edit:
  build the tree/commit objects and drive them through the same
  `receive-pack → CAS` path, vs. a direct object-write + CAS shortcut.
- **Closing the litestream window for refs** — only if the accepted lost-ack
  behaviour ever proves unacceptable (see [safety.md](safety.md) §4).
- **Read scaling.** One container serves reads and writes today; litestream read
  replicas are the later answer if reads get heavy.
