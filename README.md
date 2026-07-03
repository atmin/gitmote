# gitmote

> *Your own, personal, GitHub.*

A tiny self-hosted git remote. One Go container speaks git's smart-HTTP
protocol; repositories live in S3-compatible object storage and mutable metadata
(refs, users, keys, access) in [s3lite](https://github.com/atmin/s3lite). Use it
instead of GitHub for a handful of repos and a few invited people — it is
explicitly not trying to be GitHub at scale.

The one interesting trick: git's data is split by mutability — immutable
objects/packs to S3, mutable refs and forge metadata to s3lite (plain SQLite,
backed to S3) — so the only genuinely hard problem, atomic ref updates, becomes a
single SQL transaction.

**Status:** early implementation — git read/write over smart-HTTP works
(clone/fetch/push with token auth and per-repo ACLs), and gitmote hosts its own
source repo end-to-end: locally against MinIO (`make e2e-local`) and in
production on Scaleway Object Storage at
[gitmote.atmin.net](https://gitmote.atmin.net). The design lives in
[docs/architecture/](docs/architecture/).

## Develop locally

`make dev` gives you a running instance in one command: it builds the binaries,
starts MinIO in a container (S3 on :9100), runs gitmote **natively** on :8080,
and bootstraps an admin/token/repo on the first run. State (the metadata DB,
object cache, and the minted token) persists under `data/`, so the printed token
keeps working across restarts:

```sh
make dev        # first run prints the token, clone URL, and UI URL
make dev-reset  # wipe MinIO + data/ for a clean slate
```

Sign in to the UI at <http://localhost:8080/ui> by pasting the token, or
clone/push straight away with the printed URL. Requires Docker + Docker Compose.

## Run it locally

`make e2e-local` brings up a gitmote container plus MinIO with
[docker-compose.yml](docker-compose.yml), bootstraps an admin/token/repo, then
pushes this working tree, clones it back, and — after force-recreating the
container — clones again to prove the repo survives on the object store and
persisted refs (not local disk). Requires Docker + Docker Compose.

`make e2e-restore` additionally exercises the litestream cold-start path (wipes
the metadata DB and restores it from S3). Deployment — Scaleway Serverless
Containers, single writer, `gitmote.atmin.net` — lives in [docs/ops.md](docs/ops.md).

## Bootstrap

An empty instance has no users, so token auth is a chicken-and-egg. Run the
one-time `bootstrap` subcommand **inside the single writer** to create the first
admin, mint a token (printed once), and create the initial repo:

```sh
GITMOTE_DB=/data/meta.sqlite3 gitmote bootstrap -handle atmin -repo atmin/gitmote
```

It prints an access token exactly once — save it. Re-running is safe: it refuses
to clobber an existing admin. Then start the server (`GITMOTE_S3_BUCKET` et al.,
sharing the same `GITMOTE_DB`) and clone/push with the token:

```sh
git clone http://atmin:<token>@<host>/atmin/gitmote
```

## Management UI

The things you can't do over git — create/list repos, mint/revoke tokens, and
manage per-repo ACLs — live in a small server-rendered web UI under `/ui`. Set
`GITMOTE_COOKIE_KEY` (the HMAC key that signs session cookies) to enable it; it
runs alongside the git server. Sign in at `/login` by pasting an **admin** token
(the same PAT format git uses); the server issues a signed, stateless session
cookie. Access is limited to global admins.

---

<sub>`git push gitmote` — reach out and touch faith. 🎶</sub>
