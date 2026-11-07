# gitmote architecture

gitmote is a small Go server in a single container. The server:

- speaks git's smart-HTTP protocol
- stores repositories in S3-compatible object storage
- keeps its mutable metadata (refs, users, keys, access) in
  [s3lite](https://github.com/atmin/s3lite), which is SQLite with S3-backed
  durability

The container itself is disposable, because S3 and s3lite hold all durable state.
So gitmote runs as a single writer that scales to zero. It wakes on a request,
serves it, and goes idle again.

gitmote is for self-hosting a handful of repos. The design leaves room to invite
a few collaborators, and to accept commits authored in a web UI. gitmote does not
try to be GitHub at scale.

The design priorities, in order, are:

1. safe
2. simple
3. cheap to idle
4. fast

When two priorities conflict, the earlier one wins. A pragmatic design wins over
a pure one.

## Split git's data by mutability

Git on S3 is easy, except for one thing: atomic ref updates under concurrent
writers. All other data is immutable, or safe with a single writer. So gitmote
splits git's storage by mutability, and each kind of data goes to the store that
fits it. Real git does the parts that are hard to reimplement.

| Data | Property | Store |
| --- | --- | --- |
| Objects and packs | Immutable, content-addressed, bulky (~90% of bytes) | Plain S3: a synchronous PUT. A second PUT of the same hash is idempotent. |
| Refs, users, keys, ACLs, repo registry, web-edit state | Small, mutable, need a transactional CAS | s3lite: a ref update is one SQL transaction. |
| Everything git computes (packing, negotiation, merge-base, …) | Hard to reimplement correctly | Stock `git` on ephemeral local disk |

This split turns the one hard problem into a one-line SQL compare-and-swap. The
database is plain SQLite, through s3lite, and SQLite is already trusted in
production. There is no database to operate.

## Reading order

Read the file for the part you are about to change:

| File | Read it when |
| --- | --- |
| [storage.md](storage.md) | Before you change a component of the container, the S3 object layout or the s3lite schema. |
| [request-flows.md](request-flows.md) | Before you change the read path (clone, fetch) or the write path (push and its compare-and-swap). |
| [safety.md](safety.md) | Before you change the single writer, the content-before-pointer order or the accepted write-loss window. |
| [auth.md](auth.md) | Before you change authentication or transport: smart HTTP with personal access tokens. |
| [urls.md](urls.md) | Before you change a URL route, the repo namespace or repo visibility. |
| [ci.md](ci.md) | Before you change how a push runs `.gitmote/workflows` with `act`, or to learn why a Scaleway Job cannot build images. |
| [alternatives.md](alternatives.md) | Before you propose a different storage design, to see what gitmote already rejected. |
