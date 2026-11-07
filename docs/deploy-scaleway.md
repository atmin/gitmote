# Deploy on Scaleway

This file covers one cloud deployment: gitmote as a Scaleway Serverless Container,
deployed from GitHub Actions. [configuration.md](configuration.md) covers the
settings every deployment needs.

## Infrastructure

| Component | Provider | Product |
| --- | --- | --- |
| Compute | Scaleway | Serverless Containers in `fr-par`, with a custom domain over a CNAME and automatic TLS |
| Object storage | Scaleway | Object Storage, S3-compatible, bucket `gitmote` |
| Registry | GitHub | GHCR: the public images `ghcr.io/atmin/gitmote` (server) and `ghcr.io/atmin/gitmote-runner` (CI). Scaleway pulls both without credentials. |

The domain, such as `git.example.com`, is a CNAME to the container endpoint.

## Container settings

Set the env vars from [configuration.md](configuration.md#environment-variables) on
the container. Pass the credentials as `secret-environment-variables`, and the rest
as `environment-variables`.

> **Warning:** `scw container container update` replaces the whole
> `secret-environment-variables` map. It does not merge, so passing one secret
> removes all the others. Without the AWS keys, litestream falls back to EC2 IMDS,
> and the server crash-loops on restore. Pass every secret on every `update`. The
> plain `environment-variables` are a separate map. The CI deploy's `update` sets
> no env vars, so it keeps both maps.

**Resources:** 1000 mVCPU, 1024 MB of memory and 8 GB of ephemeral storage
(`cpu-limit=1000`, `memory-limit-bytes=1024MB`). The CLI needs a unit: `G`, `GB`
or `MB`.

- Use stays well below these limits. 256 MB was enough for `git index-pack` and
  `receive-pack`.
- The CPU limit is high to shorten a cold start. The litestream restore and the
  first git operation then run on a full core.
- The 1024 MB memory tier unlocks the 8 GB `/tmp` scratch tier. gitmote does not
  need the memory itself.

Tune the limits from the logs.

**Scale:** `min-scale = 0` lets the container idle to zero. A remote gets pushes
now and then, so it is idle almost always. Even at 1024 MB, an always-on container
exceeds Scaleway's free tier: about 2.6M GB-s per month against 400k.

- The cost of scale to zero is a cold start of a few seconds on the first request
  after idle.
- Scale-down sends a graceful SIGTERM. `Close` flushes the WAL and releases the
  lease, so no write is lost.
- Set `min-scale = 1` only when instant first-push latency is worth the always-on
  cost.

`max-scale = 1` stays, and every deploy sets it again. The lease already prevents
two writers. But a follower serves no reads, so more instances add no read
capacity.

## GitHub Actions secrets

The deploy pipeline uses Scaleway API credentials, apart from the container env.
Set them in the GitHub repo under Settings, Secrets and variables, Actions,
Repository secrets.

A missing secret does not fail where a workflow uses it: GitHub substitutes an
empty string. So a wrong name fails later, for example with
`scw … api_key not_found`.

Pushing the image needs no secret. GHCR accepts the built-in `GITHUB_TOKEN` with the
workflow's `packages: write` permission. The Scaleway secrets serve only the deploy
(`scw container update`) and the start of CI jobs.

A Scaleway API key has two halves, so do not swap them. The access key starts with
`SCW` (`SCWXXXXXXXXXXXXXXXXX`), and the secret key is a UUID. Both come from the
Organization and Project that own the container.

| Secret | Value |
| --- | --- |
| `SCW_SECRET_KEY` | The Scaleway API secret key: the UUID, not the `SCW…` access key. It deploys and starts CI jobs. |
| `SCW_ACCESS_KEY` | The Scaleway access key (`SCW…`) |
| `SCW_ORGANIZATION_ID` | The Scaleway organization ID |
| `SCW_PROJECT_ID` | The Scaleway project ID |
| `SCW_CONTAINER_ID` | The ID of the gitmote Serverless Container |

If a secret reads as empty although it is set, check these:

1. It is under Secrets, not Variables.
2. It is a Repository secret. A job with no `environment:` cannot see Environment
   secrets.
3. The name matches exactly. Names are case-sensitive.
4. For an Organization secret, the repo has access to it.

## One-time setup

```bash
# 1. Create an Object Storage bucket named "gitmote" in fr-par, in the console or
#    the CLI. Generate an API key pair for it.

# 2. Publish the first images to GHCR. CI publishes every later image, so this
#    step only seeds a new GHCR namespace. Scaleway requires amd64.
echo "<GITHUB_PAT with write:packages>" | docker login ghcr.io -u <github-user> --password-stdin
docker build  --platform=linux/amd64 -t ghcr.io/atmin/gitmote:master .
docker push   ghcr.io/atmin/gitmote:master
docker build  --platform=linux/amd64 -f Dockerfile.runner -t ghcr.io/atmin/gitmote-runner:master .
docker push   ghcr.io/atmin/gitmote-runner:master
#    Then make both packages public once, by hand: GitHub, your profile or org,
#    Packages, gitmote or gitmote-runner, Package settings, Change visibility,
#    Public. A new GHCR package is private. Until it is public, Scaleway's
#    anonymous pull fails with an error that does not name the cause.

# 3. Create the container: min-scale=0 idles to zero, max-scale=1 keeps one
#    instance. The CLI takes image= (not registry-image), cpu-limit in mVCPU,
#    memory-limit-bytes, and (secret-)environment-variables.KEY=value.
#    Only the credentials are secret. Leave the cookie key and worker secret
#    unset: gitmote generates and keeps them. The 1024 MB memory tier unlocks
#    8 GB of /tmp scratch; set the scratch size in the console.
scw container container create \
  namespace-id=<NAMESPACE_ID> \
  name=gitmote \
  image=ghcr.io/atmin/gitmote:master \
  min-scale=0 max-scale=1 \
  cpu-limit=1000 \
  memory-limit-bytes=1024MB \
  port=8080 \
  environment-variables.GITMOTE_S3_BUCKET=gitmote \
  environment-variables.GITMOTE_S3_ENDPOINT=https://s3.fr-par.scw.cloud \
  environment-variables.GITMOTE_DATA=/tmp/gitmote \
  environment-variables.AWS_REGION=fr-par \
  secret-environment-variables.AWS_ACCESS_KEY_ID=<KEY> \
  secret-environment-variables.AWS_SECRET_ACCESS_KEY=<SECRET>

# 4. Add the custom domain, in two parts.
#    (a) In DNS, add a CNAME: git.example.com to <container-endpoint>.scw.cloud.
#    (b) Register the domain on the container, so Scaleway issues a managed
#        certificate. The CNAME alone is not enough: HTTPS then serves the
#        endpoint's own certificate, and browsers report "not secure".
scw container domain create container-id=<CONTAINER_ID> hostname=git.example.com
scw container domain list   container-id=<CONTAINER_ID>   # pending, then ready after minutes
#    "/" is the web dashboard. For a liveness probe, use /healthz.
#    Git lives under /<repo>/.

# 5. Create the Serverless Job for the CI runner, with the public runner image.
#    One image serves all repos; each job gets its env at trigger. Set the new
#    definition ID as the container's SCW_CI_JOB_DEFINITION_ID to turn on cloud
#    CI. After a runner change, CI republishes the tag, and the job pulls it on
#    its next start. To change the image, update the definition:
scw jobs definition create name=gitmote-runner \
  image-uri=ghcr.io/atmin/gitmote-runner:master \
  cpu-limit=1000 memory-limit=2048
scw jobs definition update <JOB_DEFINITION_ID> image-uri=ghcr.io/atmin/gitmote-runner:master

# 6. Set the GitHub Actions secrets of the deploy pipeline (see "GitHub Actions
#    secrets"). They are not container env vars.
```

## Deploy

Every green push to `master` deploys, through the `deploy` job in
`.github/workflows/ci.yml`:

1. `ci` runs lint, test and build.
2. `e2e` runs the end-to-end suites.
3. The job builds the amd64 image, and pushes it as `ghcr.io/atmin/gitmote:master`
   and `:<sha>`.
4. `scw container container update image=ghcr.io/atmin/gitmote:<sha> … min-scale=0 max-scale=1 --wait`
   makes Scaleway pull the public image.

The deploy needs no drain step. The lease makes the rolling swap safe
([configuration.md](configuration.md#rolling-deploys)).

The runner image publishes on its own, through
`.github/workflows/publish-runner.yml`. It runs when the runner or
`Dockerfile.runner` changes, and on a release. The job definition pulls
`ghcr.io/atmin/gitmote-runner` on its next start.

### Self-deploy without GitHub

When GitHub is down, or for a deploy outside the pipeline, a local gitmote instance
builds and deploys itself. It runs
[`.gitmote/workflows/deploy.yml`](../.gitmote/workflows/deploy.yml): the GitHub
deploy job, triggered by the `self-deploy` branch instead of `master`. Only a local
instance can do this, because Scaleway cannot build.

1. Copy [`.env.example`](../.env.example) to `.env`, which git ignores.
2. Fill in the deploy secrets in `.env`.
3. Run `make self-deploy`.

`.env` holds the five Scaleway secrets of the GitHub pipeline, plus `GHCR_USER` and
`GHCR_TOKEN`. `GHCR_TOKEN` is a GHCR token with `write:packages`, because no
`GITHUB_TOKEN` exists outside GitHub.

The target does this:

- It starts a throwaway instance on `:8081`, with its own S3 server on `:9300`,
  apart from `make dev`.
- It sets `GITMOTE_CI_ALLOW_BUILDS=1`, and passes the `.env` secrets as
  `GITMOTE_REPO_SECRET_GITMOTE__<NAME>`. These secrets are never stored, and need
  no `GITMOTE_CI_SECRET_KEY`: the host env is the trust boundary
  ([safety.md §5](architecture/safety.md)).
- It pushes `HEAD` to `self-deploy`, which starts the workflow.

The target needs `act` and a running Docker daemon. The script finds the socket
from the active Docker context, so colima and Docker Desktop both work. Without a
daemon, it fails and prints the setup steps. On Apple Silicon,
`colima start --vm-type vz --vz-rosetta` speeds up `linux/amd64` builds with
Rosetta.

The build is `linux/amd64` only, the architecture of Scaleway. An arm64 host
emulates it, so the first build is slow. The build tags `:self-deploy` and `:<sha>`,
and deploys the SHA tag. It never deploys `:master`, which belongs to the GitHub
deployer. Because it runs only on `self-deploy`, it never deploys twice with the
`master` path. The lease keeps its rolling swap safe, as for the GitHub deploy.

The instance is disposable. Each run of `make self-deploy` first resets its state:
the `data/selfdeploy/` directory and its S3 volume. So an interrupted run cannot
leave litestream state that differs from the bucket. `make self-deploy-reset`
removes the state on demand.

## Check that it hosts itself

Push gitmote's own repo to the deployed instance, and clone it back:

```bash
git push https://git.example.com/gitmote HEAD:refs/heads/master   # user: admin, password: a token
git clone https://git.example.com/gitmote /tmp/gitmote-clone
git -C /tmp/gitmote-clone fsck --full                              # clean
```

To check a cold start, redeploy, or let the container idle to zero. Then clone
again. The metadata restores from the replica, and the repo serves.
`make e2e-restore` tests the same path locally.

## Troubleshooting

```bash
scw container container get <CONTAINER_ID>      # status, error messages, instance count
scw container container redeploy <CONTAINER_ID> # pull the current image tag again
docker manifest inspect ghcr.io/atmin/gitmote:master        # the server image
docker manifest inspect ghcr.io/atmin/gitmote-runner:master # the CI runner image
```

When a deploy fails on the image pull, the GHCR package is most likely private.
Make `gitmote` and `gitmote-runner` public (setup step 2).

Logs go to Scaleway Cockpit: console, Observability, Cockpit, Grafana, Explore,
Loki. gitmote logs JSON to stderr.

### Read the first token from the logs

The first start prints the admin token once. On Scaleway, that print is a log
line. This LogQL query extracts the token. A gitmote token is
`gmt_<selector>.<secret>`, and both halves are hex
([`internal/auth/token.go`](../internal/auth/token.go)).

```logql
{resource_type="serverless_container", resource_name=~"<namespace>-gitmote"} |= "gmt_" | regexp `(?P<token>gmt_[0-9a-f]+\.[0-9a-f]+)` | line_format `{{.token}}`
```

- `|= "gmt_"` filters the raw lines first. The token appears in the line as it is,
  so the query needs no `| json`.
- `regexp` captures the token, and `line_format` reduces each match to it.

Set the time range to cover the last start of the container, because the token is
minted once at start. Sort newest first, so the top line is the current token.
