# gitmote

A tiny self-hosted git remote. One Go container speaks git's smart-HTTP
protocol; repositories live in S3-compatible object storage and mutable metadata
(refs, users, keys, access) in [s3lite](https://github.com/atmin/s3lite). Use it
instead of GitHub for a handful of repos and a few invited people — it is
explicitly not trying to be GitHub at scale.

The one interesting trick: git's data is split by mutability — immutable
objects/packs to S3, mutable refs and forge metadata to s3lite (plain SQLite,
backed to S3) — so the only genuinely hard problem, atomic ref updates, becomes a
single SQL transaction.

**Status:** design stage, no code yet. The design lives in
[ARCHITECTURE.md](ARCHITECTURE.md); tasks to follow.
