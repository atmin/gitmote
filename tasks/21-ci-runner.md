# CI stage 4b — runner image + engine execution

Part of the CI epic ([16-ci.md](16-ci.md)). **Stage 4a (the internal report API)
has landed**: gitmote exposes the authenticated claim/complete endpoints, the log
store, run rollup, and the stuck-job reconciliation sweep. What remains is the
**runner** that actually executes a workflow and drives those endpoints.

> **Infra spike to resolve first:** `act` runs each job in a Docker container, so
> the runner needs a Docker daemon inside the Scaleway Serverless Job. Confirm
> Jobs expose a usable daemon (privileged / DinD) or pick the fallback: run steps
> in a single provided container without full `act` nesting. Spike this before
> building the image.

## What 4a already provides (the contract to build against)

- `GET /internal/ci/jobs/{id}` — constant-time `X-Worker-Secret` auth; atomically
  claims (`queued`→`running`) and returns the spec
  `{job_id, run_id, repo, sha, ref, workflow_dir}`. Not claimable → 404; a
  follower → retryable 503. (`internal/ci/report.go`.)
- `POST /internal/ci/jobs/{id}/complete?status=passed|failed|error` — the request
  **body is the combined log**. Content-before-pointer: the blob is stored under
  `ci/{repoID}/{runID}/{jobID}.log` (10 MiB cap, explicit truncation marker),
  then `SetJobResult` records status + `log_key`, then the run rolls up
  (running > error > failed > passed). Idempotent: a terminal job is a no-op.
- Per-run clone credential: `auth.Guard.MintScoped` (task 20) mints a read-only,
  repo-scoped, expiring token; the dispatcher already injects
  `GITMOTE_CI_CLONE_TOKEN` (currently a placeholder — **wire the real mint** here).
- `ReportAPI.ReconcileStuck(ctx, maxAge, now)` exists and is tested; **wire it to
  a leader-only periodic ticker** in `main.go` once runners can produce
  `running` jobs (nothing does until this stage).

## Change

**Wire the real clone token.** In the dispatcher (`internal/ci/dispatcher.go`),
replace the `GITMOTE_CI_CLONE_TOKEN` placeholder with a `MintScoped` call
(read-only, scoped to the pushed repo, TTL = run max-duration + margin). Revoke on
completion (or lean on expiry). Needs the dispatcher to reach the guard/mint and
a CI service user to own the token.

**Runner** (`cmd/gitmote-runner` + `Dockerfile.runner`):
- Read env (`GITMOTE_URL`, `GITMOTE_CI_JOB_ID`, `WORKER_SECRET`,
  `GITMOTE_REPO/SHA/REF`, `GITMOTE_CI_CLONE_TOKEN`); `GET` the claim.
- `git clone https://x-access-token:$TOKEN@$GITMOTE_URL/$REPO`, checkout `$SHA`
  (shallow where possible).
- Run `act` over `.github/workflows`, capturing combined stdout/stderr.
- `POST …/complete?status=…` with the captured log as the body (pass/fail from
  `act`'s exit). Exit non-zero on internal failure so a crashed runner is
  distinguishable from a failed build.
- `Dockerfile.runner`: `act` + git + the daemon strategy from the spike. One-time
  `scw jobs definition create name=gitmote-ci-runner image=… cpu-limit=…
  memory-limit=… local-storage-capacity=…` documented in
  [ops.md](../docs/ops.md); one definition serves all repos (env injected at
  trigger, per stage 3).

**Wire the reconciliation ticker.** A leader-only goroutine in `main.go` calling
`ReportAPI.ReconcileStuck` on an interval (bound ≈ run max-duration), with clean
shutdown.

## Verify

- **runner test** against a stub gitmote server with a **fake engine** (a script
  that echoes and exits 0/1): full claim→clone→run→complete flow drives the job
  to `passed`/`failed`; the reported log round-trips.
- **clone-token test:** the minted token is read-only + repo-scoped and clones the
  target repo but is denied a push and denied another repo (leans on task 20's
  guard tests).
- **security:** the runner env holds only the scoped token + `WORKER_SECRET` — no
  S3/DB credentials.
- **ticker:** a `running` job past the bound is swept to `error` by the wired
  reconciler (the sweep itself is already unit-tested in 4a).
- `gofmt`/`golangci-lint`/`go test ./...` clean; `make e2e-local` +
  `make e2e-restore` green.
