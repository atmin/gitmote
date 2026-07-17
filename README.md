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

## Quickstart — run it anywhere

One container, one bucket. S3 is the single source of truth and the container is
disposable: kill it and restart — it restores the whole forge from S3 and
continues. Point it at any S3-compatible bucket with credentials:

```sh
docker run --rm -p 8080:8080 \
  -e GITMOTE_S3_BUCKET=my-gitmote-bucket \
  -e AWS_REGION=us-east-1 \
  -e AWS_ACCESS_KEY_ID=… -e AWS_SECRET_ACCESS_KEY=… \
  ghcr.io/atmin/gitmote:master
# Published tags: :master (latest on master) and :<git-sha>. There is no :latest.
# Non-AWS S3 (Scaleway, MinIO, R2): add -e GITMOTE_S3_ENDPOINT=https://s3.fr-par.scw.cloud
# Keep the local cache across restarts (optional):  -v gitmote-data:/data
```

On the **first run** gitmote auto-bootstraps an admin and prints a one-time access
token to the logs behind a `SAVE IT NOW` banner — grab it there (no setup page).
Then sign in at <http://localhost:8080/login> by pasting it, and clone/push:

```sh
git clone http://admin:<token>@localhost:8080/<repo>   # create the repo on the dashboard (/) first
```

Lost the token? It's never stored (only its hash), so re-mint one — stop the
container, then:

```sh
docker run --rm -e GITMOTE_S3_BUCKET=… -e AWS_… ghcr.io/atmin/gitmote:master bootstrap -reissue
```

Nothing else is required: the session cookie key and CI worker secret are
generated and persisted on first run. See [docs/ops.md](docs/ops.md) for the full
env surface, CI, and the Scaleway deployment.

### Remote bucket, local on-demand forge

Because the bucket *is* the forge, you don't need any always-on compute. Keep the
bucket in the cloud, run gitmote **locally only when you want to push or pull**,
and stop it afterwards — your git remote is `localhost`, but the durable state
lives in S3. This is the cheapest way to self-host: you pay for object storage,
not a running server.

```sh
# Start the forge against your CLOUD bucket, then push/pull, then Ctrl-C.
docker run --rm -p 8080:8080 \
  -e GITMOTE_S3_BUCKET=my-gitmote-bucket \
  -e GITMOTE_S3_ENDPOINT=https://s3.fr-par.scw.cloud \  # your provider's endpoint
  -e GITMOTE_S3_PREFIX=objects \                        # MUST match the bucket's existing prefix
  -e AWS_REGION=fr-par \
  -e AWS_ACCESS_KEY_ID=… -e AWS_SECRET_ACCESS_KEY=… \
  ghcr.io/atmin/gitmote:master

git remote add gitmote http://admin:<token>@localhost:8080/<repo>
git push gitmote main
```

Two things to get right:

- **`GITMOTE_S3_PREFIX` is a data address, not a preference.** A local instance
  pointed at an existing bucket must use the **same** prefix (and endpoint/region)
  that bucket was written with, or gitmote comes up advertising refs whose objects
  it can't find and aborts startup. If you created the bucket with this same
  quickstart, match whatever you used then.
- **Use your existing token.** A populated bucket already has an admin; don't
  re-bootstrap it — sign in with the token you saved (or `bootstrap -reissue` to
  mint a fresh one).

**Only one writer at a time — and that's enforced, not your job.** gitmote takes a
single-writer *lease* on the bucket, so running a second instance (or a stray cloud
container) can't corrupt anything: whoever holds the lease writes, the other serves
reads and returns a retryable `503` on push. So a local on-demand forge is safe
even if you also run gitmote in the cloud — no coordination, no harm. Details in
[docs/ops.md](docs/ops.md) (“Single writer is a correctness invariant”).

## Develop locally

`make dev` gives you a running instance in one command: it builds the binaries,
starts MinIO in a container (S3 on :9100), runs gitmote **natively** on :8080,
and ensures an admin + a `gitmote` repo (auto-bootstrap), minting a fresh
token each run and printing it. State (the metadata DB and object cache) persists
under `data/`, so repos and history survive restarts:

```sh
make dev        # first run prints the token, clone URL, and UI URL
make dev-reset  # wipe MinIO + data/ for a clean slate
```

Sign in to the UI at <http://localhost:8080/> by pasting the token, or
clone/push straight away with the printed URL. Requires Docker + Docker Compose.

**CI works locally too.** Push a repo with `.gitmote/workflows/*.yml` and gitmote
records a run and executes it on the spot — it spawns `gitmote-runner` as a local
process (the *same* runner code the cloud path runs on Scaleway, just a local
substrate) which runs the workflow with [`act`](https://github.com/nektos/act).
Install act (`brew install act`); it uses the same Docker/podman daemon MinIO
does. Watch runs in the UI. gitmote reads `.gitmote/workflows` (GitHub-workflow
syntax, run by act) — **not** `.github/workflows`, so a repo mirrored to GitHub
chooses its forge by where the workflow lives and never double-runs.

## Run it locally

`make e2e-local` brings up a gitmote container plus MinIO with
[docker-compose.yml](docker-compose.yml), bootstraps an admin/token/repo, then
pushes this working tree, clones it back, and — after force-recreating the
container — clones again to prove the repo survives on the object store and
persisted refs (not local disk). Requires Docker + Docker Compose.

`make e2e-restore` additionally exercises the litestream cold-start path (wipes
the metadata DB and restores it from S3).

`make e2e-build` proves the `GITMOTE_CI_ALLOW_BUILDS` gate: it runs `act` in a
privileged docker-in-docker box (a rootful daemon in a box) with the daemon socket
suppressed (a `docker build` step fails) then mounted (it succeeds), so the safe
default and the opt-in are both verified against a real build. Skips cleanly with
no daemon. Deployment — Scaleway Serverless Containers, single writer,
`gitmote.atmin.net` — lives in [docs/ops.md](docs/ops.md).

## Bootstrap

An empty instance has no users, so token auth would be a chicken-and-egg — so the
server **auto-bootstraps on first run**: when it is the writer and no admin
exists, it creates the admin (`GITMOTE_ADMIN_HANDLE`, default `admin`) and prints
a one-time token to the logs (see the Quickstart). No second command.

You can still bootstrap by hand against the bucket **before** the server is live
(`gitmote bootstrap [-handle …] [-repo …]`), and recover a lost token with
`gitmote bootstrap -reissue` — both refuse to clobber and print the token once.
Only the token's hash is ever stored, so a lost token is re-minted, never
recovered.

## Web UI

The bare root `/` is a **dashboard**: a viewer-scoped repo list (public repos to
anyone, private to those with access, all to an admin) with browse pages for each
repo. Sign in at `/login` by pasting a token (the same PAT format git uses) — any
user may sign in to see the repos they can access; the server issues a signed,
stateless session cookie keyed by an auto-generated, persisted secret (override
with `GITMOTE_COOKIE_KEY`).

The things you can't do over git are **admin-only**: create repos and mint/revoke
tokens from the top-level `/users` and `/tokens` pages, and manage a repo from its
`/<repo>/settings` (visibility, default branch), `/<repo>/access` (invite
spectators/collaborators), and `/<repo>/secrets` pages.

---

<sub>`git push gitmote` — reach out and touch faith. 🎶</sub>
