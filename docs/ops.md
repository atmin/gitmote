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

The invariant is now enforced **by a lease, not by procedure.** The server opens
its metadata in s3lite `RoleAuto`: it becomes the writer only while it holds a
lease (litestream's `s3.Leaser` — an `If-None-Match`/`If-Match` conditional-write
CAS on `lock.json` in the same bucket, now that Scaleway supports conditional
writes), and runs as a read-only follower otherwise. gitmote gates receive-pack
on `IsLeader()`, so a follower refuses pushes with a retryable `503` while still
serving clones/fetches.

- **Rolling deploys are safe by construction.** Scaleway's rolling deploy briefly
  runs old + new together (observed: instance count spikes to 2 in Cockpit). The
  new instance boots as a **follower** (the old still holds the lease); the old
  releases on its graceful SIGTERM (`Close` flushes, then releases the lease
  last); the new then **promotes** on its next lease poll. Never two writers — no
  drain step, no `POST /admin/quit`. Brief handoff window: the new instance is up
  read-only for ≤ ~lease-TTL/3 after the old exits, so a push in that gap gets the
  retryable `503`; reads are unaffected. (After a *hard* kill the successor waits
  out the ≤30 s lease TTL before acquiring — a graceful exit releases at once.)
- **`max-scale = 1`** still holds, re-asserted on every deploy. Not for the
  writer invariant anymore (the lease covers that) but because a follower serves
  a *restored snapshot* and polls — it is not a continuously-fresh read replica —
  so scaling out for read throughput needs follower-freshness work first. Raising
  it is the tracked follow-up ([reader-writer-split](evolution/reader-writer-split.md));
  until then keep it at 1. `min-scale = 0` (idle to zero) is orthogonal — cost/latency only.
- By the content-before-pointer ordering ([safety.md §3](architecture/safety.md)),
  even a hypothetical overlap could only leak orphan objects (garbage `gc`
  reclaims), never a ref pointing at a missing object.

The [safety.md §4](architecture/safety.md) escape hatch (refs behind a
conditional-PUT CAS on object storage) is likewise possible on Scaleway now if
ever wanted; gitmote's ref CAS remains a SQL transaction in the metadata DB
(unchanged — it never needed an S3 precondition).

## Runtime env vars (on the container)

These configure the **running server** and are set **on the Scaleway Serverless
Container** (via `scw container container create/update`, secrets as
`secret-environment-variables`). They are **not** the CI credentials — the deploy
pipeline's Scaleway API keys are separate GitHub Actions secrets (next section).
Setting these on the container does nothing for CI, and vice versa.

> ⚠️ **`update` replaces the entire `secret-environment-variables` map — it does
> not merge.** Passing one secret wipes the rest (observed: setting a single
> secret dropped the AWS keys, so litestream fell back to EC2 IMDS and the server
> crash-looped on restore). **Always pass *all* secret env vars together on every
> `update`.** Plain `environment-variables` are a separate map, unaffected. The CI
> deploy's `update` sets no env vars, so it preserves both maps.

| Variable | Value / meaning |
|----------|-----------------|
| `GITMOTE_S3_BUCKET` | `gitmote` — a bucket alone derives the metadata replica (`s3://gitmote/meta`) and the single-writer lease |
| `GITMOTE_S3_ENDPOINT` | `https://s3.fr-par.scw.cloud` |
| `GITMOTE_DATA` | `/tmp/gitmote` — base dir for the db (`meta.sqlite3`), cache, and socket; ephemeral, restored from S3 on cold start |
| `GITMOTE_DB_REPLICA` | optional override for the derived `s3://{bucket}/meta` replica target |
| `GITMOTE_HOOK` | pre-receive hook binary (defaults to `gitmote-hook` beside the server) |
| `GITMOTE_RUNNER` | CI runner binary the local trigger spawns (defaults to `gitmote-runner` beside the server) |
| `GITMOTE_COOKIE_KEY` | secret — signs management-UI session cookies (enables `/ui`) |
| `GITMOTE_URL` | public base URL (`https://gitmote.atmin.net`) — injected into the CI runner's env so it clones and reports back here |
| `SCW_CI_JOB_DEFINITION_ID` | the Scaleway Serverless Job definition for the CI runner. **Set → cloud trigger** (Scaleway job per CI job) |
| `SCW_SECRET_KEY` | secret — Scaleway API secret key (the UUID) used to start the CI job; also the registry/deploy key |
| `SCW_REGION` | Scaleway region for the CI job start (falls back to `AWS_REGION`) |
| `WORKER_SECRET` | secret — shared runner-auth secret; injected into the runner env and compared on its report-back |
| `GITMOTE_CI_SECRET_KEY_V1` | secret — base64 of 32 bytes; master key for per-repo CI secrets. Add `_V2`, … to rotate (highest is current; old envelopes still decrypt). Unset → the secrets UI is disabled and none are injected |
| `AWS_REGION` | `fr-par` |
| `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` | secret — Scaleway API key pair; covers both the object store and the litestream replica |

> **CI trigger selection.** The dispatcher always records runs/jobs; what executes
> them depends on the env. With `SCW_CI_JOB_DEFINITION_ID` set, the **Scaleway**
> trigger starts a Serverless Job per CI job (cloud); it then requires
> `WORKER_SECRET` and `GITMOTE_URL`, and the server refuses to start without them.
> With that unset but `GITMOTE_URL` + `WORKER_SECRET` present, the **local** trigger
> spawns `gitmote-runner` as a local process — the *same runner code and env
> contract*, a local substrate instead of a cloud job (this is what `make dev`
> uses). With none of them set, runs record but nothing executes. The runner runs
> `.github/workflows` with `act`, which needs a reachable Docker daemon. A
> leader-only ticker sweeps jobs stuck `running` past ~1h back to `error`.

`GITMOTE_DATA` is ephemeral on purpose: the object store + litestream replica are
the durable state, and the local disk is a cache. On a cold start (scale-to-zero →
wake, or a redeploy) the DB is restored from the derived replica and repos
re-materialize from the bucket. This path is proven locally by `make e2e-restore`
(wipes the data volume, confirms the repo still clones).

Resources: **250 mVCPU / 512 MB / 2 GB ephemeral** (`cpu-limit=250`,
`memory-limit-bytes=512MB` — the CLI requires a `G`/`GB`/`MB` unit). Observed
usage sits well under these; 256 MB was already plenty for `git index-pack` /
`receive-pack`. Memory is set to 512 MB only to unlock the larger ephemeral
scratch tier (2 GB `/tmp`), not for RAM headroom. Tune from logs.

Scale: **`min-scale = 0`** (idle to zero). A remote pushed to occasionally is idle
almost always, and even at 512 MB always-on exceeds Scaleway's free tier
(~2.6M GB-s/mo vs 400k), so scale-to-zero is the cheaper default. The cost is a few seconds of
cold start on the first request after idle (restore + materialize, the path above);
scale-down is a graceful SIGTERM, so the shutdown `Close` durably flushes the WAL
(and releases the writer lease) — no lost writes. Set `min-scale = 1` only if
instant first-push latency is worth the always-on cost. `max-scale` stays **1**
regardless (see the single-writer section — follower reads aren't fresh enough to
scale out yet).

## CI secrets (GitHub Actions)

Separate from the container env above: these are the Scaleway API credentials the
deploy pipeline uses, set in the **GitHub repo** → Settings → Secrets and variables
→ **Actions** → **Repository secrets**. A missing one doesn't error at reference
time — GitHub substitutes an empty string, so a wrong name surfaces later as e.g.
`docker login` → *"Password required"*.

A Scaleway API key has two halves — don't swap them: the **access key** is
`SCWXXXXXXXXXXXXXXXXX` (starts with `SCW`), the **secret key** is a UUID
(`xxxxxxxx-xxxx-…`). The registry password is the **secret key**. Both must come
from the same Organization/Project as the `atmin` registry namespace, or registry
login fails with `api_key … not_found`.

| Secret | Value |
|--------|-------|
| `SCW_SECRET_KEY` | Scaleway API **secret key** — the UUID, not the `SCW…` access key (registry login + deploy) |
| `SCW_ACCESS_KEY` | Scaleway **access key** (`SCW…`) |
| `SCW_REGISTRY_ENDPOINT` | `rg.fr-par.scw.cloud/atmin` |
| `SCW_ORGANIZATION_ID` | Scaleway organization ID |
| `SCW_PROJECT_ID` | Scaleway project ID |
| `SCW_CONTAINER_ID` | the gitmote Serverless Container ID |

If a secret reads as empty despite being set, check: it's under **Secrets** not
**Variables**; it's a **Repository** secret (a job with no `environment:` can't see
Environment secrets); the name matches exactly (case-sensitive); and, if kept as an
**Organization** secret, the `gitmote` repo is granted access to it.

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
#    Arg names per the current CLI: image= (not registry-image), cpu-limit (mVCPU),
#    memory-limit-bytes, and key-based (secret-)environment-variables.KEY=value.
#    Only credentials and the cookie key are secret; the rest are plain
#    environment-variables. The ephemeral /tmp scratch tier is coupled to the
#    memory tier — 512 MB unlocks 2 GB scratch (set in the console).
scw container container create \
  namespace-id=<NAMESPACE_ID> \
  name=gitmote \
  image=rg.fr-par.scw.cloud/atmin/gitmote:master \
  min-scale=0 max-scale=1 \
  cpu-limit=250 \
  memory-limit-bytes=512MB \
  port=8080 \
  environment-variables.GITMOTE_S3_BUCKET=gitmote \
  environment-variables.GITMOTE_S3_ENDPOINT=https://s3.fr-par.scw.cloud \
  environment-variables.GITMOTE_DATA=/tmp/gitmote \
  environment-variables.AWS_REGION=fr-par \
  secret-environment-variables.GITMOTE_COOKIE_KEY="$(openssl rand -base64 32)" \
  secret-environment-variables.AWS_ACCESS_KEY_ID=<KEY> \
  secret-environment-variables.AWS_SECRET_ACCESS_KEY=<SECRET>
# GITMOTE_COOKIE_KEY is container-only, so a fresh random inline is fine for it.

# 4. Custom domain — two parts:
#    (a) DNS: add a CNAME  gitmote.atmin.net → <container-endpoint>.scw.cloud
#    (b) Register the domain ON the container so Scaleway issues a managed cert
#        (the CNAME alone is not enough — until this, HTTPS serves the endpoint's
#        own cert and browsers report "not secure"):
scw container domain create container-id=<CONTAINER_ID> hostname=gitmote.atmin.net
scw container domain list   container-id=<CONTAINER_ID>   # status pending → ready (minutes)
#    Note: gitmote has no route at "/", so http(s)://gitmote.atmin.net/ returns 404
#    by design; check /healthz for liveness. Git lives under /<owner>/<repo>/….

# 5. Set the GitHub Actions repository secrets for the deploy pipeline — these are
#    the CI credentials, NOT container env vars. See "CI secrets (GitHub Actions)".
```

## Bootstrap (first run is automatic)

An empty instance has no admin/token. **On first start the server auto-bootstraps
itself**: when it is the writer and no admin exists, it creates the admin (handle
from `GITMOTE_ADMIN_HANDLE`, default `admin`), mints a token, and prints it once
to the logs behind an unmissable banner. So a fresh `docker run` against an empty
bucket is usable without a second command — just grab the token from the logs.

The token **transits the logs**, which are operator-visible (same trust boundary
as the env and bucket). Treat it as a one-time credential: after first sign-in,
mint your own token in the UI and revoke the bootstrap one (see below).

No initial repo is created — make repos in the UI in two clicks.

### Manual bootstrap / token recovery

Both run **from your machine against the bucket** so you are transiently the
single writer — do this *before* the container serves traffic (or while it is
scaled to zero). They open in `RoleWriter`: acquire the writer lease and fail
loudly if the server already holds it, so they can never race the live writer.

```bash
# Common env for both:
export GITMOTE_S3_BUCKET=gitmote \
       GITMOTE_S3_ENDPOINT=https://s3.fr-par.scw.cloud \
       GITMOTE_DATA=/tmp/bootstrap \
       AWS_REGION=fr-par AWS_ACCESS_KEY_ID=<KEY> AWS_SECRET_ACCESS_KEY=<SECRET>

# Bootstrap by hand (optional -repo). Idempotent — refuses to clobber an admin:
gitmote bootstrap -handle atmin

# Lost the token? Mint a fresh one for the existing admin (safe: whoever can run
# this against the bucket already has total infra control):
gitmote bootstrap -reissue -handle atmin
```

Each prints the access token once (save it) and durably flushes the new state to
`s3://gitmote/meta` on `Close`; the server restores it on next start. Only the
token's hash is ever stored — the raw token is never recoverable, hence `-reissue`.

## Deploying

Automatic on every green push to `master` (`.github/workflows/ci.yml`, `deploy`
job): `ci` (lint/test/build) → `e2e` (local push/clone + litestream restore) →
build+push the amd64 image → `scw container container update … min-scale=0
max-scale=1 --wait`. No drain step: the writer lease makes the rolling deploy
safe by construction (new boots as a follower, old releases on SIGTERM, new
promotes — see the single-writer section).

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
