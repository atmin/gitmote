# Operations

Canonical infra doc — resources, env, deploy, and the one operational rule. Keep
it current whenever infra or env changes.

gitmote is `gitmote.atmin.net` — a subdomain of atmin.net by DNS only. It is
otherwise independent; it currently happens to run on Scaleway Serverless
Containers like atmin.net, but that is incidental and may change.

## Infrastructure

| Component | Provider | Product |
|-----------|----------|---------|
| Compute | Scaleway | Serverless Containers (`fr-par`, custom domain via CNAME, auto TLS) |
| Object storage | Scaleway | Object Storage (S3-compatible), bucket `gitmote` |
| Registry | GitHub | GHCR — **public** `ghcr.io/atmin/gitmote` (server) + `ghcr.io/atmin/gitmote-runner` (CI). Scaleway pulls both anonymously |

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
CAS on `lock.json` in the same bucket), and runs as a read-only follower
otherwise. This makes **atomic conditional writes a hard requirement on the S3
provider**: the lease — and therefore the single-writer correctness invariant —
holds only if the backend supports the compare-and-swap primitive (`If-None-Match`
to acquire, `If-Match` to renew/release). Most do (AWS S3, Scaleway, …); one that
doesn't cannot safely back gitmote. gitmote gates receive-pack on `IsLeader()`, so
a follower refuses pushes with a retryable `503` while still serving clones/fetches.

- **Rolling deploys are safe by construction.** A rolling deploy briefly runs a
  second container alongside the old one — two instances live at once — which does
  **not** violate the single-writer invariant. The new instance boots as a
  **follower** (the old still holds the lease); the old releases on its graceful
  SIGTERM (`Close` flushes, then releases the lease last); the new then
  **promotes** on its next lease poll. Never two writers — no drain step, no
  `POST /admin/quit`. Brief handoff window: the new instance is up
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
conditional-PUT CAS on object storage) would rely on the same conditional-write
primitive; gitmote's ref CAS remains a SQL transaction in the metadata DB
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

**Required — the whole core.** A bucket plus its credentials is a complete forge.

| Variable | Value / meaning |
|----------|-----------------|
| `GITMOTE_S3_BUCKET` | `gitmote` — a bucket alone derives the metadata replica (`s3://gitmote/meta`) and the single-writer lease |
| `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` | secret — S3 key pair; covers both the object store and the litestream replica (Scaleway API key on prod) |
| `AWS_REGION` | `fr-par` (any valid region for non-AWS S3) |
| `GITMOTE_S3_ENDPOINT` | `https://s3.fr-par.scw.cloud` — the S3 endpoint; **omit for real AWS S3** |

**Auto-managed — leave unset; generated and persisted in meta on first run (an
explicit env still wins).** No longer part of the required surface.

| Variable | Value / meaning |
|----------|-----------------|
| `GITMOTE_COOKIE_KEY` | signs management-UI session cookies; auto-generated + persisted (stable across restart / scale-to-zero) |
| `WORKER_SECRET` | runner report-back auth; auto-generated + persisted **when a CI trigger is configured** (below) |

**CI — optional, off by default** (runs are still recorded; nothing executes until
one of these turns a trigger on — see [CI substrate](#ci-substrate)).

| Variable | Value / meaning |
|----------|-----------------|
| `SCW_CI_JOB_DEFINITION_ID` | the Scaleway Serverless Job definition. **Set → cloud trigger** (a Scaleway job per CI job); requires `GITMOTE_URL` |
| `GITMOTE_URL` | public base URL (`https://gitmote.atmin.net`) the runner clones + reports back to; required for any CI. **Set alone → local `act` trigger** |
| `SCW_SECRET_KEY` | secret — Scaleway API secret key (UUID) to start the job (the same key also deploys the container from CI) |
| `SCW_REGION` | region for the job start (falls back to `AWS_REGION`) |
| `GITMOTE_CI_SECRET_KEY_V<n>` | secret — base64 of 32 bytes; **env-only** master key for per-repo CI secrets, never persisted ([safety.md §5](architecture/safety.md)). Add `_V2`, … to rotate (highest is current; old envelopes still decrypt). Unset → the secrets UI is off and none are injected |

**Advanced overrides — rarely needed** (the defaults are derived).

| Variable | Value / meaning |
|----------|-----------------|
| `GITMOTE_DATA` | base dir for the db (`meta.sqlite3`), cache, and socket; **preset to `/data` in the image** — mount a volume there to keep the cache across restarts. Ephemeral by design (restored from S3 on cold start) |
| `GITMOTE_DB_REPLICA` | override the derived `s3://{bucket}/meta` replica target |
| `GITMOTE_DB` / `GITMOTE_CACHE` / `GITMOTE_SOCK` | override the individual paths that otherwise derive from `GITMOTE_DATA` |
| `GITMOTE_S3_PREFIX` | key prefix for git objects inside the bucket (does **not** affect the `meta` replica path) |
| `GITMOTE_HOOK` / `GITMOTE_RUNNER` | hook / CI-runner binary paths (default beside the server) |
| `GITMOTE_ADMIN_HANDLE` | first-run auto-bootstrap admin handle (default `admin`) |

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

## CI substrate

gitmote always records a run per pushed workflow; **what executes it is chosen
from the env at startup** (one runner, three substrates — see
[ci.md](architecture/ci.md)):

1. `SCW_CI_JOB_DEFINITION_ID` set → **Scaleway Serverless Jobs** (cloud): a job per
   CI job. Also requires `GITMOTE_URL` (the runner clones + reports back there);
   `WORKER_SECRET` is auto-provisioned. The server refuses to start if the job
   definition is set without `GITMOTE_URL`.
2. else `GITMOTE_URL` set → **local `act`**: the server spawns `gitmote-runner` as a
   local process — the *same* runner code and env contract, a local substrate
   instead of a cloud job (this is what `make dev` uses).
3. else → **disabled**: runs and jobs are recorded, but nothing executes.

**Prerequisite for both executing substrates:** the runner runs `.github/workflows`
with [`act`](https://github.com/nektos/act), which needs a reachable **Docker or
podman daemon** — in the cloud that's the Serverless Job image; locally it's the
daemon on the host (the same one MinIO uses under `make dev`). A leader-only ticker
sweeps jobs stuck `running` past ~1h back to `error`.

## CI secrets (GitHub Actions)

Separate from the container env above: these are the Scaleway API credentials the
deploy pipeline uses, set in the **GitHub repo** → Settings → Secrets and variables
→ **Actions** → **Repository secrets**. A missing one doesn't error at reference
time — GitHub substitutes an empty string, so a wrong name surfaces later as e.g.
`scw … api_key not_found`.

The image push itself needs **no secret**: publishing to GHCR uses the built-in
`GITHUB_TOKEN` with the workflow's `packages: write` permission. The Scaleway
secrets below are only for the `scw container update` deploy (and starting CI
jobs). A Scaleway API key has two halves — don't swap them: the **access key** is
`SCWXXXXXXXXXXXXXXXXX` (starts with `SCW`), the **secret key** is a UUID
(`xxxxxxxx-xxxx-…`); both must come from the gitmote Organization/Project.

| Secret | Value |
|--------|-------|
| `SCW_SECRET_KEY` | Scaleway API **secret key** — the UUID, not the `SCW…` access key (deploy + start CI job) |
| `SCW_ACCESS_KEY` | Scaleway **access key** (`SCW…`) |
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

# 2. Publish the initial images to GHCR, then flip both packages to PUBLIC once by
#    hand (GitHub → your profile/org → Packages → gitmote / gitmote-runner →
#    Package settings → Change visibility → Public). New GHCR packages default to
#    private; until public, Scaleway's anonymous pull fails with an error the code
#    can't diagnose. CI republishes on every push (server) and on runner changes,
#    so this manual build is only the very first seed. Scaleway requires amd64.
echo "<GITHUB_PAT with write:packages>" | docker login ghcr.io -u atmin --password-stdin
docker build  --platform=linux/amd64 -t ghcr.io/atmin/gitmote:master .
docker push   ghcr.io/atmin/gitmote:master
docker build  --platform=linux/amd64 -f Dockerfile.runner -t ghcr.io/atmin/gitmote-runner:master .
docker push   ghcr.io/atmin/gitmote-runner:master

# 3. Create the container (single writer: min-scale=0 idle-to-zero, max-scale=1).
#    Arg names per the current CLI: image= (not registry-image), cpu-limit (mVCPU),
#    memory-limit-bytes, and key-based (secret-)environment-variables.KEY=value.
#    Only the credentials are secret; the rest are plain environment-variables. The
#    cookie key and worker secret are NOT set — they auto-generate and persist. The
#    ephemeral /tmp scratch tier is coupled to the memory tier — 512 MB unlocks 2 GB
#    scratch (set in the console).
scw container container create \
  namespace-id=<NAMESPACE_ID> \
  name=gitmote \
  image=ghcr.io/atmin/gitmote:master \
  min-scale=0 max-scale=1 \
  cpu-limit=250 \
  memory-limit-bytes=512MB \
  port=8080 \
  environment-variables.GITMOTE_S3_BUCKET=gitmote \
  environment-variables.GITMOTE_S3_ENDPOINT=https://s3.fr-par.scw.cloud \
  environment-variables.GITMOTE_DATA=/tmp/gitmote \
  environment-variables.AWS_REGION=fr-par \
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
#    by design; check /healthz for liveness. Git lives under /<repo>/….

# 5. CI runner Serverless Job — point its definition at the public GHCR runner
#    image (one image serves all repos; per-job env is injected at trigger). Set
#    the resulting definition ID as the container's SCW_CI_JOB_DEFINITION_ID to
#    turn on the cloud CI trigger. Update the image the same way after a runner
#    change (CI republishes the tag; the Job pulls it on next start):
scw jobs definition create name=gitmote-runner \
  image-uri=ghcr.io/atmin/gitmote-runner:master \
  cpu-limit=1000 memory-limit=2048
scw jobs definition update <JOB_DEFINITION_ID> image-uri=ghcr.io/atmin/gitmote-runner:master

# 6. Set the GitHub Actions repository secrets for the deploy pipeline — these are
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
build+push the public amd64 image to `ghcr.io/atmin/gitmote:{master,<sha>}` →
`scw container container update image=ghcr.io/atmin/gitmote:<sha> … min-scale=0
max-scale=1 --wait` (Scaleway pulls the public image). No drain step: the writer
lease makes the rolling deploy safe by construction (new boots as a follower, old
releases on SIGTERM, new promotes — see the single-writer section).

The **runner** image publishes separately on runner/Dockerfile.runner changes (or
a release) via `.github/workflows/publish-runner.yml` →
`ghcr.io/atmin/gitmote-runner`; the CI Job definition pulls it on next start.

```bash
git push origin master
```

## Verifying it hosts itself

```bash
# Push the gitmote repo over HTTPS to the deployed instance:
git push https://atmin:<token>@gitmote.atmin.net/gitmote HEAD:refs/heads/master
git clone https://atmin:<token>@gitmote.atmin.net/gitmote /tmp/gitmote-clone
git -C /tmp/gitmote-clone fsck --full   # clean
```

Cold-start check: trigger a redeploy (or let it idle to zero and wake it), then
clone again — the metadata restores from the replica and the repo serves. The
same path is exercised locally by `make e2e-restore`.

## Troubleshooting

```bash
scw container container get <CONTAINER_ID>      # status, error messages, instance count
scw container container redeploy <CONTAINER_ID> # re-pull the current image tag
# Images live in GHCR now — inspect/pull them there:
docker manifest inspect ghcr.io/atmin/gitmote:master        # server image
docker manifest inspect ghcr.io/atmin/gitmote-runner:master # CI runner image
```

If a deploy fails on an image pull, the most likely cause is a **private** GHCR
package: flip `gitmote` / `gitmote-runner` to public (see one-time setup step 2).

Logs flow to Scaleway Cockpit (console → Observability → Cockpit → Grafana →
Explore → Loki). gitmote logs JSON to stderr.
