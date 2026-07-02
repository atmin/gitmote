# Operations

Canonical infra doc — resources, env, deploy, and the one operational rule. Keep
it current whenever infra or env changes.

gitmote runs on the same account and host as atmin.net (see that repo's
`docs/ops.md` for the Scaleway pattern in more depth); this doc is gitmote-specific.

## Infrastructure

| Component | Provider | Product |
|-----------|----------|---------|
| Compute | Scaleway | Serverless Containers (`fr-par`, custom domain via CNAME, auto TLS) |
| Object storage | Scaleway | Object Storage (S3-compatible), bucket `gitmote` |
| Registry | Scaleway | Container Registry (`rg.fr-par.scw.cloud/atmin`, shared with atmin.net) |

Reachable at **`gitmote.atmin.net`** (CNAME → the container endpoint). EU-resident
infrastructure, consistent with atmin.net's stance.

One bucket, two prefixes: `objects/` (immutable git objects/packs) and `meta/`
(the litestream WAL of the metadata SQLite). Refs are the source of truth and
live in the metadata DB; objects live under `objects/`.

## ⚠️ Single writer is a correctness invariant

[safety.md §1](architecture/safety.md) — **never two writer instances.** gitmote
replicates its metadata SQLite to S3 with litestream; two instances writing the
same WAL can corrupt the replica. This is stronger than a stateless service's
in-process-race concern: it is a data-integrity precondition.

- **`max-scale = 1`** on the container, asserted on every deploy (the CI deploy
  step sets it; it is not left to a console default). Autoscaling above 1 opens
  the forbidden state. `min-scale = 0` (idle to zero) is fine — it only changes
  cost/latency, not the invariant; `max-scale = 1` is what guards it.
- **No two instances during a transition.** Each deploy pushes a uniquely
  (SHA-)tagged image and `update … --wait`s for the new deployment to serve. With
  `max-scale = 1` Scaleway should replace the single instance rather than run two;
  the same must hold when waking from zero. **Confirm on the first deploy and the
  first wake-from-idle** (watch `scw container container get` — instance count
  must not exceed 1). If Scaleway is ever seen to overlap, switch the deploy to
  scale the container to zero, wait for termination, then bring up the new image.
- Because pushes are operator-initiated and infrequent, deploy while no push is
  in flight. The content-before-pointer ordering ([safety.md §3](architecture/safety.md))
  bounds any residual window to orphan objects (garbage `gc` reclaims), never a
  ref pointing at a missing object.

Note: Scaleway Object Storage has **no conditional writes** (`If-Match` /
`If-None-Match`). This does **not** affect gitmote — the ref CAS is a SQL
transaction in the metadata DB, not an S3 precondition. (It does rule out the
safety.md §4 escape hatch of moving refs to a conditional-PUT on S3 *while on
Scaleway*; that would need a provider with preconditions.)

## Runtime env vars

Set on the Scaleway Serverless Container (secrets as
`secret-environment-variables`):

| Variable | Value / meaning |
|----------|-----------------|
| `GITMOTE_ADDR` | bind address — `:8080` (Scaleway routes to `port=8080`) |
| `GITMOTE_S3_BUCKET` | `gitmote` |
| `GITMOTE_S3_ENDPOINT` | `https://s3.fr-par.scw.cloud` |
| `GITMOTE_S3_PREFIX` | `objects/` — git objects live under this prefix |
| `GITMOTE_DB_REPLICA` | `s3://gitmote/meta` — litestream restore + backup target |
| `GITMOTE_DB` | `/tmp/gitmote/meta.sqlite3` — ephemeral; restored from the replica on cold start |
| `GITMOTE_CACHE` | `/tmp/gitmote/cache` — ephemeral; materialized repos rebuild from S3 |
| `GITMOTE_SOCK` | `/tmp/gitmote/gitmote.sock` — pre-receive hook RPC socket |
| `GITMOTE_COOKIE_KEY` | secret — signs management-UI session cookies (enables `/ui`) |
| `AWS_REGION` | `fr-par` |
| `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` | secret — Scaleway API key pair; covers both the object store and the litestream replica |

`GITMOTE_DB` and `GITMOTE_CACHE` are ephemeral on purpose: the object store +
litestream replica are the durable state, and the local disk is a cache. On a
cold start (scale-to-zero → wake, or a redeploy) the DB is restored from
`GITMOTE_DB_REPLICA` and repos re-materialize from `objects/`. This path is
proven locally by `make e2e-restore` (wipes the DB volume, confirms the repo
still clones).

Memory: **1 GB** (`memory-limit-bytes=1GB` — the CLI requires a `G`/`GB` unit).
`git index-pack` / `receive-pack` need far more than a stateless signer; 1 GB
gives headroom and, at `min-scale=0`, is idle-billed. Tune from logs.

Scale: **`min-scale = 0`** (idle to zero). A remote pushed to occasionally is idle
almost always, and always-on at 512 MB exceeds Scaleway's free tier (~1.3M GB-s/mo
vs 400k), so scale-to-zero is the cheaper default. The cost is a few seconds of
cold start on the first request after idle (restore + materialize, the path above);
scale-down is a graceful SIGTERM, so the shutdown `Sync` flushes the WAL — no lost
writes. Set `min-scale = 1` only if instant first-push latency is worth the
always-on cost. `max-scale` stays **1** regardless (single-writer invariant).

## One-time Scaleway setup

```bash
# 1. Object Storage bucket (console or CLI), fr-par, name "gitmote".
#    Generate an API key pair for it.

# 2. Push the initial image (registry namespace "atmin" is shared with atmin.net;
#    Scaleway requires amd64 — build with --platform on ARM Macs).
docker login rg.fr-par.scw.cloud/atmin -u nologin -p <SCW_SECRET_KEY>
docker build --platform=linux/amd64 -t rg.fr-par.scw.cloud/atmin/gitmote:master .
docker push rg.fr-par.scw.cloud/atmin/gitmote:master

# 3. Create the container (single writer: min-scale=0 idle-to-zero, max-scale=1).
#    Arg names per the current CLI: image= (not registry-image), memory-limit-bytes,
#    and key-based (secret-)environment-variables.KEY=value. Only credentials and
#    the cookie key are secret; the rest are plain environment-variables.
scw container container create \
  namespace-id=<NAMESPACE_ID> \
  name=gitmote \
  image=rg.fr-par.scw.cloud/atmin/gitmote:master \
  min-scale=0 max-scale=1 \
  memory-limit-bytes=1GB \
  port=8080 \
  environment-variables.GITMOTE_S3_BUCKET=gitmote \
  environment-variables.GITMOTE_S3_PREFIX=objects/ \
  environment-variables.GITMOTE_S3_ENDPOINT=https://s3.fr-par.scw.cloud \
  environment-variables.GITMOTE_DB_REPLICA=s3://gitmote/meta \
  environment-variables.GITMOTE_DB=/tmp/gitmote/meta.sqlite3 \
  environment-variables.GITMOTE_CACHE=/tmp/gitmote/cache \
  environment-variables.GITMOTE_SOCK=/tmp/gitmote/gitmote.sock \
  environment-variables.AWS_REGION=fr-par \
  secret-environment-variables.GITMOTE_COOKIE_KEY="$(openssl rand -base64 32)" \
  secret-environment-variables.AWS_ACCESS_KEY_ID=<KEY> \
  secret-environment-variables.AWS_SECRET_ACCESS_KEY=<SECRET>

# 4. Custom domain — two parts:
#    (a) DNS: add a CNAME  gitmote.atmin.net → <container-endpoint>.scw.cloud
#    (b) Register the domain ON the container so Scaleway issues a managed cert
#        (the CNAME alone is not enough — until this, HTTPS serves the endpoint's
#        own cert and browsers report "not secure"):
scw container domain create container-id=<CONTAINER_ID> hostname=gitmote.atmin.net
scw container domain list   container-id=<CONTAINER_ID>   # status pending → ready (minutes)
#    Note: gitmote has no route at "/", so http(s)://gitmote.atmin.net/ returns 404
#    by design; check /healthz for liveness. Git lives under /<owner>/<repo>/….

# 5. GitHub secrets for the deploy pipeline (same set as atmin.net):
#    SCW_ACCESS_KEY, SCW_SECRET_KEY, SCW_ORGANIZATION_ID, SCW_PROJECT_ID,
#    SCW_REGISTRY_ENDPOINT (rg.fr-par.scw.cloud/atmin), SCW_CONTAINER_ID.
```

## Bootstrap (run once, before the server is live)

An empty instance has no admin/token. Bootstrap **from your machine against the
prod bucket** so you are transiently the single writer — do this *before* the
container serves traffic (or while it is scaled to zero), never as a job while
the server runs, or that is two writers.

```bash
GITMOTE_DB=/tmp/bootstrap.sqlite3 \
GITMOTE_DB_REPLICA=s3://gitmote/meta \
GITMOTE_S3_ENDPOINT=https://s3.fr-par.scw.cloud \
AWS_REGION=fr-par AWS_ACCESS_KEY_ID=<KEY> AWS_SECRET_ACCESS_KEY=<SECRET> \
  gitmote bootstrap -handle atmin -repo atmin/gitmote -default-branch master
```

It prints the access token once (save it) and `Sync`s the new state to
`s3://gitmote/meta`; the server restores it on first start. Re-running is safe —
it refuses to clobber an existing admin.

## Deploying

Automatic on every green push to `master` (`.github/workflows/ci.yml`, `deploy`
job): `ci` (lint/test/build) → `e2e` (local push/clone + litestream restore) →
build+push the amd64 image → `scw container container update … min-scale=0
max-scale=1 --wait`.

```bash
git push origin master
```

## Verifying it hosts itself

```bash
# Push the gitmote repo over HTTPS to the deployed instance:
git push https://atmin:<token>@gitmote.atmin.net/atmin/gitmote HEAD:refs/heads/master
git clone https://atmin:<token>@gitmote.atmin.net/atmin/gitmote /tmp/gitmote-clone
git -C /tmp/gitmote-clone fsck --full   # clean
```

Cold-start check: trigger a redeploy (or let it idle to zero and wake it), then
clone again — the metadata restores from the replica and the repo serves. The
same path is exercised locally by `make e2e-restore`.

## Troubleshooting

```bash
scw container container get <CONTAINER_ID>      # status, error messages, instance count
scw container container redeploy <CONTAINER_ID> # re-pull the current image tag
scw registry image list                          # images in the registry
```

Logs flow to Scaleway Cockpit (console → Observability → Cockpit → Grafana →
Explore → Loki). gitmote logs JSON to stderr.
