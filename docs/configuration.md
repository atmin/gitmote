# Configuration

This file covers what every gitmote deployment needs:

- the storage root
- the single writer
- the env vars
- CI
- the first admin

[deploy-scaleway.md](deploy-scaleway.md) covers one cloud deployment.

## Storage root

gitmote keeps all its state under one storage root. The root is a whole bucket,
or a `bucket/base` path inside a shared bucket (`GITMOTE_S3_BUCKET`). The root
holds:

- `{repo}/objects/`: the immutable git objects and packs
- `ci/`: the logs of CI runs
- `meta/`: the litestream replica of the metadata SQLite database, and the lease
  file `lock.json`

Refs live in the metadata database, which is their source of truth.

## Single writer

Two instances must never write at the same time. Both would write to the same
litestream WAL, and that can corrupt the replica.
[safety.md §1](architecture/safety.md) explains the rule.

A lease enforces the rule. gitmote opens its metadata in s3lite `RoleAuto`. An
instance writes only while it holds the lease, and runs as a read-only follower
otherwise. The lease is litestream's `s3.Leaser`: a compare-and-swap on
`{root}/meta/lock.json`. It uses `If-None-Match` to acquire the lease, and
`If-Match` to renew or release it.

So the S3 provider must support atomic conditional writes. AWS S3 and Scaleway
do. A provider without them cannot safely back gitmote.

`leaderGate` splits requests into reads and writes:

- A write calls `s3lite.TryPromote` at once on a follower. With the lease, it
  restores the latest state and serves. Without it, the write gets a retryable
  `503`.
- A read never takes the lease. A follower catches up with `s3lite.Refresh`, then
  serves. The read lags the writer by under a second.

A write is a push, a web UI POST other than sign-in and sign-out, or a CI report
call. Liveness probes and static assets need neither the lease nor the catch-up.

### Two instances take turns

An instance keeps the lease only while it is busy. After 5 s (`idleYieldAfter`)
with no request in flight, it yields the lease with `s3lite.YieldLease`. The
yield syncs first and deletes the lock last, so the next writer sees every
acknowledged write. Followers promote only on demand (`OnDemandPromotion`), never
in a background poll. So the free lease goes to the instance that gets the next
request.

This lets a local container and a cloud instance share one storage root:

- A write to one instance while the other is busy gets the `503`. It succeeds
  once the other has been idle for 5 s. A read succeeds at once.
- The first write after an idle period takes the lease. The instance resumes in
  place when nobody else wrote, and restores when someone did.
- The repo cache rewrites its refs from the metadata database on every request.
  So it never serves a ref that the other instance moved or deleted.
- The CI reconcile sweep takes the lease through the same gate every 5 minutes.

### Rolling deploys

A rolling deploy runs the new instance beside the old one for a short time. This
keeps the single writer:

1. The new instance starts as a follower, because the old one holds the lease.
2. On a graceful SIGTERM, the old instance's `Close` flushes the WAL and releases
   the lease last.
3. The first write to the new instance takes the free lease. `TryPromote` blocks
   for at most `promoteTimeout` while it restores, then serves. Reads on the new
   instance never wait for the lease.

A graceful deploy needs no drain step and returns no `503`: the first write waits
for the restore. After a hard kill, the lease stays held until its TTL of at most
30 s ends. Writes in that window get the retryable `503`, and reads succeed.

### Writes are ordered

gitmote writes objects before it moves a ref
([safety.md §3](architecture/safety.md)). So even an overlap of two writers could
only leave orphan objects. They take space but do no harm, and gitmote never
deletes them. A ref never points
at a missing object.

The ref compare-and-swap is a SQL transaction in the metadata database, and needs
no S3 precondition.

## Environment variables

Set these on the container or process that runs the server. A deploy pipeline has
its own credentials, apart from these.

**Required.** A storage root and its credentials make a complete forge.

| Variable | Meaning |
| --- | --- |
| `GITMOTE_S3_BUCKET` | The storage root: a bucket such as `gitmote`, or `bucket/base` for a path in a shared bucket. The metadata replica (`{root}/meta`) and the lease derive from it. |
| `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` | Secret. The S3 key pair, for the object store and the litestream replica. Any source of the AWS SDK's default credential chain works too, such as a file named by `AWS_SHARED_CREDENTIALS_FILE`. |
| `AWS_REGION` | The region. The default is `us-east-1`, which most S3-compatible stores ignore. Set it for AWS S3, and on Scaleway, for example `fr-par`. |
| `GITMOTE_S3_ENDPOINT` | The S3 endpoint, such as `https://s3.fr-par.scw.cloud`. Leave it unset for AWS S3. |

**Defaults.** Leave these unset unless you need another value. A value in the env
always wins.

| Variable | Meaning |
| --- | --- |
| `GITMOTE_COOKIE_KEY` | Signs the session cookies of the web UI. By default, gitmote generates it on the first start and keeps it in the metadata database. So it survives restarts and scale to zero. |
| `WORKER_SECRET` | Authenticates the reports of the CI runner. By default, gitmote generates it and keeps it in the metadata database when a CI trigger is on. |
| `GITMOTE_DATA` | The directory for the database (`meta.sqlite3`). It also holds the cache and the socket. The image sets `/data`. Elsewhere, the default is `gitmote` in the system temp directory. Mount a volume on it to keep the cache across restarts. |
| `GITMOTE_LISTEN_ADDR` | The address the server listens on. The default is `:8080`. |

The data directory is a cache. The object store and the litestream replica hold
the durable state. Cold starts include wakes from scale to zero and redeploys. On a
cold start, gitmote restores the database from the replica. Then it rebuilds the
repos from the bucket. `make e2e-restore` tests this path.

**CI.** CI is off by default. gitmote still records runs, but nothing executes them
until one of these variables turns a trigger on (see [CI](#ci)).

| Variable | Meaning |
| --- | --- |
| `SCW_CI_JOB_DEFINITION_ID` | The Scaleway Serverless Job definition. When set, each CI job runs as a Scaleway job. It needs `GITMOTE_URL`. |
| `GITMOTE_URL` | The public base URL, such as `https://git.example.com`, where the runner clones and reports. Every CI trigger needs it. Set without a job definition, it runs jobs locally with `act`. |
| `SCW_SECRET_KEY` | Secret. The Scaleway API secret key, a UUID, that starts the jobs. A deploy uses the same key. |
| `SCW_REGION` | The region where jobs start. The default is `AWS_REGION`. |
| `GITMOTE_CI_SECRET_KEY_V<n>` | Secret. The master key for per-repo CI secrets: base64 of 32 bytes. It lives only in the env, and gitmote never stores it ([safety.md §5](architecture/safety.md)). To rotate it, add `_V2` and so on. The highest number is current, and old envelopes still decrypt. Without a key, the secrets UI is off and jobs get no secrets. |
| `GITMOTE_REPO_SECRET_<REPO>__<NAME>` | A CI secret for one repo, set in the env. gitmote never stores it, and it needs no master key: the host env is already the trust boundary ([safety.md §5](architecture/safety.md)). |
| `GITMOTE_CI_ALLOW_BUILDS` | `1` or `true` lets workflows build container images. It works on a local or VPS runner with a Docker daemon (see [Image builds](#image-builds)). Use it with trusted repos only ([safety.md §7](architecture/safety.md)). It has no effect on Scaleway. |
| `GITMOTE_ACT_DAEMON_SOCKET` | The Docker socket path, as the daemon sees it, that jobs mount when builds are on. The default is `/var/run/docker.sock`. This default fits native Docker and colima. It also fits Docker Desktop. Change it only for another daemon, such as rootless Docker. |

## CI

gitmote records a run for each workflow in a push. The env at startup chooses what
executes the run ([ci.md](architecture/ci.md)):

1. With `SCW_CI_JOB_DEFINITION_ID` set, each CI job runs as a Scaleway Serverless
   Job. This also needs `GITMOTE_URL`, and the server refuses to start without it.
   gitmote generates `WORKER_SECRET`.
2. Otherwise, with `GITMOTE_URL` set, the server runs `gitmote-runner` as a local
   process. The runner code and its env are the same as on Scaleway. `make dev`
   uses this trigger.
3. Otherwise, CI is off. gitmote records runs and jobs, but nothing executes them.

The runner runs `.gitmote/workflows` with [act](https://github.com/nektos/act).
On Scaleway, `act` runs in self-hosted mode, so the steps run directly in the Job
container, with no daemon. The local trigger needs a Docker or podman daemon on
the host. There, `act` runs each job in a container.

Without a daemon, every local run fails visibly. gitmote dispatches and records
the job. Then `act` fails at its "Set up job" step. The error is
`check if … the daemon is running`. No workflow step runs, and no build runs. A
ticker on the writer marks a job as `error` when it stays `running` for about an
hour.

### Image builds

With `GITMOTE_CI_ALLOW_BUILDS` set, `act` keeps its default mount of the host
Docker socket in each job container. A workflow step such as `docker build` or
`docker/build-push-action` then reaches the daemon and builds images, gitmote's
own included. The flag is off by default, because it gives untrusted workflow code
the daemon, which is host root ([safety.md §7](architecture/safety.md)). Turn it on
only on a host that runs trusted repos.

The runner follows `DOCKER_HOST`. Plain Docker needs nothing more. Podman must
expose its Docker API socket:

```sh
systemctl --user enable --now podman.socket
export DOCKER_HOST=unix://$XDG_RUNTIME_DIR/podman/podman.sock
```

A podman without a daemon cannot back `act`. Scaleway ignores the flag, because it
has no daemon.

## First admin

An empty storage root has no users. When the server holds the lease and finds no
admin, it creates the user `admin`. It mints a token and prints it once in the logs.
So a new forge needs no setup command.

The token passes through the logs. The operator can read the logs, as they can
read the env and the bucket. Treat the token as a one-time credential. After the
first sign-in, mint your own token in the UI and revoke the first one.

gitmote creates no repository. Create repositories in the UI.

### Bootstrap by hand, and recover a token

`gitmote bootstrap` creates the admin by hand. `gitmote bootstrap -reissue` mints
a new token for the existing admin. Anyone who can run it against the bucket
already controls the forge, so it adds no risk.

Both commands open the metadata in `RoleWriter`. They take the lease, and fail when
a server holds it, so they never race a live writer. Run them while no instance is
busy:

- before the server starts
- while it is scaled to zero
- after 5 s of idle

With the compose file from the [README](../README.md), stop the server and run
the command in a one-off container:

```sh
docker compose stop
docker compose run --rm gitmote bootstrap -reissue
docker compose start
```

To run the binary, give it the settings of the server. Keep the keys in a
credentials file, not in the shell:

```sh
export GITMOTE_S3_BUCKET=gitmote \
       GITMOTE_S3_ENDPOINT=https://s3.fr-par.scw.cloud \
       AWS_REGION=fr-par \
       AWS_SHARED_CREDENTIALS_FILE=$HOME/.config/gitmote/s3-credentials \
       GITMOTE_DATA=/tmp/bootstrap

gitmote bootstrap -handle admin            # -repo is optional; it never replaces an admin
gitmote bootstrap -reissue -handle admin   # a new token for the existing admin
```

Each command prints the token once, so save it. On exit, it flushes the new state
to `{root}/meta`. The server reads that state when it next takes the lease.
gitmote stores only a hash of each token. So gitmote can mint a new token, but it
cannot show a lost one.
