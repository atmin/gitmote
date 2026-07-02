# Rejected / deferred alternatives

> Part of the [gitmote architecture](README.md).

- **Dumb HTTP remote** (S3 as a static git remote). No smarts needed for fetch,
  but no protocol negotiation (transfers too much) and awkward pushes. Too weak
  for "instead of GitHub."
- **S3 as a filesystem** (`s3fs`, `mountpoint-s3`, JuiceFS). The mounts simple
  enough to stay tiny lack the atomic rename git relies on (`mountpoint-s3`
  historically had no rename at all); JuiceFS provides real POSIX but needs its
  own metadata service — no longer tiny; `rclone mount --vfs-cache-mode full`
  works only by caching to local disk and writing back, which _is_ this design in
  disguise.
- **Native S3 object backend** (JGit DFS / gitoxide ODB). The "pure" version —
  git objects/packs served straight from S3 with no local materialization. Proven
  on the JVM (Gerrit), but overkill at this scale, and gitoxide's server side is
  still immature. Running stock git on ephemeral disk buys correctness for free.
