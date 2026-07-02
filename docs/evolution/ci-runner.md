# CI runner (speculative)

> **Status: evolution note — speculative, not a commitment.** Where gitmote could
> go, captured so the reasoning survives a fresh session. Nothing here is planned
> or specced; don't cite it as a spec.

## The idea

Once gitmote hosts repos, the natural graduation is CI: run workflows on push,
the way GitHub Actions does — in a limited but real form. The payoff isn't
feature parity; it's **inverting the source of truth.** S3 holds the truth,
gitmote + a serverless runner execute your code, and GitHub becomes a plain
`git push` mirror — a second remote, not the origin of anything. Self-hosting
stops being a downgrade.

## Why it fits the existing design

CI doesn't bolt on; it reuses seams already built:

| CI need               | Reused gitmote primitive                                                                                                                                            |
| --------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Trigger (`on: push`)  | The s3lite ref **CAS commit** — the one durable "a ref advanced" moment ([ARCHITECTURE.md](../../ARCHITECTURE.md) → Push flow). "After a successful CAS, enqueue a run." |
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
idle" shape as gitmote itself. A job spins up, clones from gitmote (it speaks
git), runs the engine, reports back, dies. (Serverless Containers for anything
long-lived or interactive.)

## Precedent

**Forgejo / Gitea Actions** is a small self-hosted forge running an
Actions-compatible runner (`act_runner`, built on act's engine). "Small forge +
reused engine + Actions compat" ships today — this isn't hypothetical.

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

- Which engine — `act` (Actions compat) vs Woodpecker (simplicity). Decided by
  whether Actions compatibility actually matters to you.
- Workflow file location and format.
- Concurrency: runs are _dispatched_ by the single writer but _execute_ in
  parallel on Scaleway; only result-reporting funnels back through the writer.
