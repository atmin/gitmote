# gitmote — Architecture

A tiny single-container Go server that speaks git's smart-HTTP protocol, stores
repositories in S3-compatible object storage, and keeps its mutable metadata
(refs, users, keys, access) in [s3lite](https://github.com/atmin/s3lite)
(SQLite with S3-backed durability). The container itself is disposable — all
durable truth lives in S3 + s3lite — so it runs as a **single writer that scales
to zero**: it wakes on a request, serves, and idles back down.

Built for self-hosting a handful of repos, with the door open to invite a few
collaborators and to accept commits authored through a web UI. It is explicitly
**not** trying to be GitHub at scale.

Design priorities, in order: **safe → simple → cheap-to-idle → fast.** Where
they conflict, the earlier one wins. Pragmatic beats pure.

---

## The core idea: split git's data by mutability

"Git on S3" is easy except for one thing — **atomic ref updates under
concurrent writers**. Everything else is either immutable or single-writer-safe.
So we split git's storage along the axis that decides which store each half
wants, and we let _real git_ do the parts that are hard to reimplement:

| Data                                                                   | Property                                            | Store                                                                 |
| ---------------------------------------------------------------------- | --------------------------------------------------- | --------------------------------------------------------------------- |
| Objects & packs                                                        | Immutable, content-addressed, bulky (~90% of bytes) | **Plain S3** — synchronous PUT, re-PUT of the same hash is idempotent |
| Refs, users, keys, ACLs, repo registry, web-edit state                 | Small, mutable, need transactional CAS              | **s3lite** — a ref update is one SQL transaction                      |
| Everything git actually computes (packing, negotiation, merge-base, …) | Hard to reimplement correctly                       | **Stock `git`** on ephemeral local disk                               |

The payoff: the single genuinely-hard problem collapses into a one-line SQL
compare-and-swap, on a database already trusted in production — plain SQLite (via
s3lite) — with no database to operate.

---

## Reading order

The rest of the architecture is split by concern:

| Doc | What's in it |
| --- | --- |
| [storage.md](storage.md) | The components that make up the container, and where each kind of data lives — S3 object layout and the s3lite schema. |
| [request-flows.md](request-flows.md) | The read path (clone/fetch) and the write path (push, and the compare-and-swap that makes it safe). |
| [safety.md](safety.md) | The concurrency & safety model — single writer, the content-before-pointer ordering invariant, and the accepted write-loss window. |
| [auth.md](auth.md) | Authentication and transport — smart HTTP + personal access tokens today, SSH deferred. |
| [ci.md](ci.md) | CI — running `.gitmote/workflows` on push: the dispatch seam, the one-runner-three-substrates model, `act` self-hosted vs. nested, secrets, and the no-in-Job-image-builds limitation. |
| [alternatives.md](alternatives.md) | Approaches considered and rejected or deferred, with the reasoning. |
| [open-questions.md](open-questions.md) | Unsettled decisions — the superset that links into [`../notes/`](../notes/). |

Speculative, non-committal future directions live in
[`../evolution/`](../evolution/) — read them as ideas, not plans.
