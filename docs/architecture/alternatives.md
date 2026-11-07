# Rejected alternatives

> Part of the [gitmote architecture](README.md).

## Dumb HTTP remote

S3 serves as a static git remote. A fetch then needs no server logic. But there
is no protocol negotiation, so a fetch transfers too much, and pushes are
awkward. This is too weak to use instead of GitHub.

## S3 as a filesystem

The candidates are `s3fs`, `mountpoint-s3` and JuiceFS.

- The mounts that are simple enough to stay tiny lack the atomic rename that git
  relies on. Early versions of `mountpoint-s3` have no rename at all.
- JuiceFS provides real POSIX semantics, but it needs its own metadata service.
  With that service, the server is not tiny.
- `rclone mount --vfs-cache-mode full` works only because it caches to local disk
  and writes back. That is this design in another form.

## Native S3 object backend

Examples are JGit DFS and the gitoxide ODB. This is the pure version: git serves
objects and packs straight from S3, with no local materialization. It is proven
on the JVM, in Gerrit. But it is more than this scale needs, and the server side
of gitoxide is immature. Stock git on ephemeral disk gives correctness with no
extra work.
