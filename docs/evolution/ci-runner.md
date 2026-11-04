# CI runner (speculative)

> **Status: largely realized.** [architecture/ci.md](../architecture/ci.md)
> describes what exists. This note keeps the original reasoning, so do not cite it
> as a spec.
>
> - CI runs `.gitmote/workflows` on push, with `act` self-hosted on Scaleway Jobs.
>   It keeps logs, and has a status UI and encrypted secrets.
> - Container image builds work on a local or VPS runner with a daemon, behind the
>   `GITMOTE_CI_ALLOW_BUILDS` opt-in. The opt-in is off by default. It grants CI
>   the host daemon, which is root, so it suits trusted repos only. So a gitmote
>   on a VPS can build its own image.
> - A local instance runs the self-deploy loop below (build, push, redeploy) as a
>   fallback. The latest-wins guard below is not built.
> - The Scaleway substrate cannot build: it has no daemon and no user namespaces.
>   So cloud deployment stays on GitHub Actions.

## The idea

With repos hosted on gitmote, the next step is CI. It runs workflows on push, as
GitHub Actions does, in a limited but real form. The goal is not feature parity.
The goal is to invert the source of truth:

- S3 holds the truth.
- gitmote and a serverless runner execute your code.
- GitHub becomes a plain `git push` mirror: a second remote, not the origin of
  anything.

Then self-hosting is not a downgrade.

The full loop goes further: a push runs CI and redeploys gitmote. So gitmote
hosts, builds and ships its own next version.

An instance that replaces itself mid-write risks two writers. The leased writer
([reader-writer-split](reader-writer-split.md)) exists, and makes this safe by
construction:

1. The new image boots as a follower.
2. The old instance releases the lease on SIGTERM.
3. The new instance promotes.

So the loop never has two writers.

The GitHub mirror then has a second job: recovery. A bad push could break the
self-deploy loop. Then GitHub holds an independent, clean copy, and an operator
redeploys from it by hand. So the mirror is also a backup. This recovery path
makes the mirror a requirement, not a nicety.

## Why it fits the existing design

CI reuses parts that gitmote already has:

| CI need | Reused gitmote primitive |
| --- | --- |
| Trigger (`on: push`) | The s3lite ref CAS commit: the one durable moment when a ref advances ([push flow](../architecture/request-flows.md)). After a successful CAS, enqueue a run. |
| Run and job state | An s3lite `runs` / `jobs` table. A single writer is fine at this scale. |
| Logs and artifacts | Append-only blobs in S3. Content-before-pointer applies unchanged. |
| Runner report to the server | The parent's authenticated API, with the same rules as the push-hook channel. Runners never touch s3lite directly. |
| Status UI | Another view of forge metadata. |

## Drive an existing engine

gitmote does not rebuild the runner. It drives an existing engine, just as it
shells out to git and does not reimplement git. The candidates:

- [act](https://github.com/nektos/act) runs GitHub-Actions YAML in containers. It
  is the path to Actions compatibility.
- [Woodpecker CI](https://woodpecker-ci.org) is tiny and container-native, with a
  simple pipeline format. It is the path to a lean native format.

gitmote supplies the forge around the engine, not the executor.

## Runner substrate: Scaleway Serverless Jobs

Serverless Jobs fit the runner closely. A Job is a finite task that scales to
zero. It has the same shape as gitmote itself: wake, work, idle. It also offers
CPU that the Container cannot. A Job has no Docker daemon, so `act` runs
self-hosted in the Job container.

A Job runs these steps:

1. It starts.
2. It clones from gitmote, which speaks git.
3. It runs the engine.
4. It reports back.
5. It exits.

The writer stays on Serverless Containers. Jobs suit long-running batch work, and
a sibling project already uses them for it.

## Precedent

Forgejo and Gitea Actions are the precedent. There, a small self-hosted forge runs
an Actions-compatible runner: `act_runner`, built on the engine of act. So a small
forge with a reused engine and Actions compatibility ships today. It is not
hypothetical.

## Trigger and deploy rules

- **Thin, fire-and-forget trigger.** An after-commit callback dispatches the run
  after the ref CAS commits. It never blocks the push. A failed dispatch is a
  missed deploy, not a failed push: log it and move on. This is the same rule as
  content-before-pointer. The user's push must succeed without the deploy
  machinery.
- **Latest-wins on deploy.** Rapid pushes race. The lease keeps the race safe,
  because there are never two writers. But a slow run could still ship an older
  SHA after a newer one. The guard: the deploy step does nothing unless its target
  SHA is still the branch tip in gitmote. So a stale run deploys nothing, and
  production does not regress.

## Caveats

- **Limited is correct.** Full Actions compatibility is a large surface: the
  marketplace, matrix builds, service containers, caching and OIDC. But about 80%
  of personal CI is "on push, run steps in a container, pass/fail, keep logs".
  That part is within reach. Start constrained. Grow compatibility only where the
  lack of it hurts.
- **Secrets are the one new security surface.** CI injects deploy keys and tokens
  into the runner env. They need a new store, encrypted at rest (AES-256-GCM) and
  separate from everything else. The rest of CI reuses existing primitives, but
  this part needs real care.

## Engine choice

gitmote uses `act`, not Woodpecker. The mirror decides the choice:

- `act` runs the YAML of `.github/workflows`, and is about 80% compatible with
  Actions. So one CI definition runs both on gitmote and on the GitHub mirror.
  The recovery redeploy then uses the same workflow. Here, Actions compatibility
  is a requirement, not a preference.
- Woodpecker descends from Drone, and has its own `.woodpecker.yml` format with no
  Actions compatibility. Its simplicity means two CI definitions:
  `.woodpecker.yml` for gitmote, and `.github/workflows/` for the mirror. The two
  would drift, and quietly weaken the guarantee of GitHub as a clean redeploy
  source.

Woodpecker fits only where identical CI on the mirror does not matter.

## Workflow files

Workflows live in `.gitmote/workflows`, in GitHub-Actions YAML.
[architecture/ci.md](../architecture/ci.md) gives the reason for the directory.

## Concurrency

The single writer dispatches runs, but the runs execute in parallel on Scaleway.
Only the result reports pass back through the writer. The latest-wins deploy
guard would keep parallel runs from deploying an older SHA to production.
