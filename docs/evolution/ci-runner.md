# CI runner (speculative)

> **Status: largely realized — see [architecture/ci.md](../architecture/ci.md).**
> CI (run `.github/workflows` on push, `act` self-hosted on Scaleway Jobs, logs,
> status UI, encrypted secrets) has landed. The one part **dropped**: the
> self-deploy loop below — the Serverless Job sandbox can't build container
> images (no user namespaces / privileged mode), so gitmote does not rebuild its
> own image; deployment stays on GitHub Actions. This note is kept for the
> original reasoning; don't cite it as a spec.

## The idea

Once gitmote hosts repos, the natural graduation is CI: run workflows on push,
the way GitHub Actions does — in a limited but real form. The payoff isn't
feature parity; it's **inverting the source of truth.** S3 holds the truth,
gitmote + a serverless runner execute your code, and GitHub becomes a plain
`git push` mirror — a second remote, not the origin of anything. Self-hosting
stops being a downgrade.

The loop then closes on itself: a push runs CI **and redeploys gitmote** —
gitmote hosting, building, and shipping its own next version. That used to be the
scary case (an instance replacing itself mid-write is the two-writer hazard), but
the **leased writer** ([reader-writer-split](reader-writer-split.md), now shipped)
makes it safe by construction: the new image boots as a follower, the old releases
the lease on SIGTERM, the new promotes — the ouroboros never has two writers. The
GitHub mirror then earns a second job: **break-glass.** If a bad push ever wedges
the self-deploy loop, GitHub is an independent clean copy to redeploy from by
hand — a backup that happens to also be a mirror. That recovery path is *why* the
mirror is load-bearing, not a nicety.

## Why it fits the existing design

CI doesn't bolt on; it reuses seams already built:

| CI need               | Reused gitmote primitive                                                                                                                                            |
| --------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Trigger (`on: push`)  | The s3lite ref **CAS commit** — the one durable "a ref advanced" moment ([architecture → push flow](../architecture/request-flows.md)). "After a successful CAS, enqueue a run." |
| Run / job state       | An s3lite `runs` / `jobs` table — single-writer is fine at this scale                                                                                                |
| Logs & artifacts      | Append-only blobs in S3; content-before-pointer applies unchanged                                                                                                   |
| Runner → server report| The parent's authenticated API — same discipline as the push-hook channel; runners never touch s3lite directly                                                      |
| Status UI             | Another forge-metadata view                                                                                                                                          |

## Don't rebuild the runner — drive an existing engine

Same move as "don't reimplement git, shell out to it." Candidates:

- **[act](https://github.com/nektos/act)** — runs GitHub-Actions YAML in
  containers. The path to Actions compatibility.
- **[Woodpecker CI](https://woodpecker-ci.org)** — tiny, container-native, simple
  pipeline format. The path to a lean native format.

gitmote supplies the forge around the engine, not the executor.

## Runner substrate: Scaleway Serverless Jobs

Almost purpose-built: a finite task that scales to zero — the same "wake, work,
idle" shape as gitmote itself, and with a real Docker daemon + CPU the Container
can't offer. A job spins up, clones from gitmote (it speaks git), runs the engine,
reports back, dies. (Serverless Containers stay for the always-warm writer;
long-running batch work is exactly what Jobs are for — already proven in a sibling
project.)

## Precedent

**Forgejo / Gitea Actions** is a small self-hosted forge running an
Actions-compatible runner (`act_runner`, built on act's engine). "Small forge +
reused engine + Actions compat" ships today — this isn't hypothetical.

## Trigger & deploy discipline

- **Thin, fire-and-forget trigger.** The post-receive hook dispatches the run
  *after* the ref CAS commits and never blocks the push on it. A failed dispatch
  is a **missed deploy, not a failed push** — log it and move on. Same rule as
  content-before-pointer: the user's push succeeding must not depend on the deploy
  machinery.
- **Latest-wins on deploy.** Rapid pushes race; the lease keeps that *safe* (never
  two writers), but a slow run could still ship an older SHA after a newer one.
  Guard: the deploy step no-ops unless its target SHA is still the branch tip in
  gitmote — a stale run deploys nothing rather than regressing prod.

## Honest caveats

- **Limited is correct.** Full Actions compatibility (the marketplace, matrix
  builds, service containers, caching, OIDC) is a huge surface. But ~80% of
  personal CI is "on push, run steps in a container, pass/fail, keep logs" — very
  reachable. Start constrained; grow compatibility only where the lack is felt.
- **Secrets are the one genuinely new security surface.** CI needs deploy
  keys / tokens injected into the runner env — a new store, encrypted at rest
  (AES-256-GCM), separate from everything else. The rest of CI reuses primitives
  that already exist; this part needs real care.

## Open threads

- Which engine — `act` (runs `.github/workflows` YAML, ~80% Actions-compatible)
  vs Woodpecker (Drone-lineage, its own `.woodpecker.yml` format, no Actions
  compat). Not a neutral toss-up given the mirror: `act` keeps **one** CI
  definition that runs both self-hosted *and* on the GitHub mirror, so the
  break-glass redeploy path uses the same workflow — Actions-compat is
  **load-bearing here, not a preference.** Woodpecker's simplicity means **two**
  CI definitions (`.woodpecker.yml` for gitmote, `.github/workflows/` for the
  mirror) that drift, quietly weakening the "GitHub as clean redeploy" guarantee.
  Pick Woodpecker only if the mirror running identical CI genuinely doesn't matter.
- Workflow file location and format.
- Concurrency: runs are _dispatched_ by the single writer but _execute_ in
  parallel on Scaleway; only result-reporting funnels back through the writer. The
  latest-wins deploy guard (above) keeps parallel runs from racing prod backwards.
