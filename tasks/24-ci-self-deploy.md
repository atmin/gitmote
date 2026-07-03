# CI stage 7 — self-deploy loop + latest-wins guard

Part of the CI epic ([16-ci.md](16-ci.md)). The capstone: a green CI run on
`master` **redeploys gitmote**, closing the ouroboros. Depends on the full CI
pipeline (stages 1–6). The leased writer already makes an instance replacing
itself safe by construction ([ops.md](../docs/ops.md), the single-writer section)
— this stage adds the trigger and the anti-regression guard.

## Spec

On a green run for `master`, deploy the new image. **Latest-wins:** the deploy
no-ops unless its target SHA is still `master`'s tip in gitmote, so a slow/stale
run never regresses prod (pairs with stage 6's `superseded` marking). The GitHub
mirror runs the **same** `.github/workflows` (engine is `act`, epic §Stage 0 #1),
so it doubles as **break-glass**: an independent clean copy to redeploy from by
hand if a bad push ever wedges the self-deploy loop.

## Current

- Today's deploy is GitHub Actions (`.github/workflows/ci.yml`) → build+push the
  amd64 image → `scw container container update … --wait` ([ops.md](../docs/ops.md)
  "Deploying"). No drain step (the lease covers it).
- With `act`, that **same workflow** now also runs self-hosted on gitmote CI —
  one definition, two runners (self + mirror).
- gitmote exposes `master`'s current tip via the ref advertisement
  (`info/refs`) / the metadata layer.

## Change

**1. Deploy job in the workflow.** Keep the existing build+push+`scw … update`
steps (they already work on GitHub Actions and, via `act`, self-hosted). No new
deploy mechanism — reuse it.

**2. Latest-wins guard.** Before the `scw … update` step, check that the run's
target SHA still equals `master`'s tip in gitmote (query `info/refs` or a tiny
read endpoint for the branch SHA); if not, **skip the deploy** and log that a
newer commit superseded this run — deploy nothing rather than ship an older SHA.
Prefer implementing the guard as a workflow step (portable to both runners) over
server-side logic, so the mirror enforces it too.

**3. Docs.** Update [ops.md](../docs/ops.md) "Deploying" for the self-hosted path
and the latest-wins guard, and add a **break-glass** note: if the self-deploy loop
is wedged, redeploy by hand from the GitHub mirror (the same workflow, clean
copy). Add a new [safety.md](../docs/architecture/safety.md) subsection tying the
loop to the leased-writer invariant (new boots follower, old releases on SIGTERM,
new promotes — never two writers). Cross-link from
[evolution/ci-runner.md](../docs/evolution/ci-runner.md) (mark the loop realized).

## Verify

- **latest-wins guard test:** a run whose SHA is still the tip proceeds; a run
  whose SHA is **no longer** the tip deploys **nothing** (unit-test the guard's
  decision against a stubbed tip; integration if a runner harness exists).
- **safety regression:** `make e2e-restore` (cold-start restore) and
  `make e2e-local` (push/clone + restart durability) stay green — the redeploy
  path relies on the same lease behavior they already prove; no two-writer window
  is introduced.
- **mirror parity:** the same `.github/workflows` runs on both GitHub Actions and
  the self-hosted runner without divergence (the load-bearing reason `act` was
  chosen).
- `gofmt`/`golangci-lint`/`go test ./...` clean.

## Not in scope

Deploy environments/approvals, rollbacks beyond "redeploy the previous image
tag", and multi-service deploys. The loop here is gitmote deploying gitmote.
