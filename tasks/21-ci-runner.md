# CI stage 4 — runner image + internal report API

Part of the CI epic ([16-ci.md](16-ci.md)). Depends on **stages 1–3
([17](17-ci-data-model.md)–[19](19-ci-scaleway-trigger.md))** (runs/jobs +
trigger with env) and the **scoped-token precursor ([20](20-ci-scoped-tokens.md))**
(per-run clone credential). This is the stage that actually **executes** a
workflow. It is the largest stage and may split into 4a (internal API) and 4b
(runner image) during implementation.

> **Infra unknown to resolve first (blocks 4b, not 4a):** `act` runs each job in
> a Docker container, so the runner needs a Docker daemon inside the Scaleway
> Serverless Job. Confirm Jobs expose a usable daemon (privileged / DinD) or pick
> the fallback: run steps in a single provided container without full `act`
> nesting. Spike this before building the image; the internal API (4a) does not
> depend on it.

## Spec

A runner process, launched as a Scaleway Serverless Job with the stage-3 env,
**claims** its job from gitmote, **clones** the repo at the SHA using the per-run
read-only scoped token, **runs `act`** over `.github/workflows`, streams the
combined log back, and **reports completion**. gitmote exposes an authenticated
internal API for claim + completion, secured by a constant-time `WORKER_SECRET`
compare. **Runners never touch s3lite or S3 directly** — logs and status flow
through the API (the parent writes the `ci/` log blob), matching the push-hook
discipline.

## Current

- The dispatcher triggers a job with `GITMOTE_CI_JOB_ID`, `GITMOTE_URL`,
  `WORKER_SECRET`, `GITMOTE_REPO/SHA/REF`, `GITMOTE_CI_CLONE_TOKEN` (stages 3+20).
- The **template** is pan0's `cmd/worker/main.go` +
  `server/handle_internal_jobs.go`: `GET /internal/jobs/{id}` (claim, header
  `X-Worker-Secret`) → run → `POST /internal/jobs/{id}/complete` with
  `{status,error,outputs}`, and an **idempotent** `ApplyCompletion` (no-op if the
  job is already terminal). Steal the shape.
- gitmote already speaks git-HTTPS; a scoped token (stage 20) is a valid clone
  credential on the normal auth path — no special-casing in the git handler.
- Log layout + caps are locked (epic §Stage 0 #5): `ci/{repoID}/{runID}/{jobID}.log`,
  10 MiB cap with an explicit truncation marker, 30-day retention.

## Change

**4a. Internal report API** (new `internal/ci` handlers, mounted on the main mux,
**not** under `/ui`):
- `GET /internal/ci/jobs/{id}` — constant-time `X-Worker-Secret`
  (`crypto/subtle`); atomically claim (`queued`→`running`); return the job spec
  `{repo, sha, ref, workflowDir}`.
- `POST /internal/ci/jobs/{id}/complete` — `{status, error}` + the log as the
  body (or a multipart field). **Content-before-pointer:** enforce the 10 MiB cap
  (truncate-with-marker), `store.Put` the log under the `ci/` key, **then**
  `SetJobStatus` + `log_key` and roll the run status up. Idempotent: a second
  complete for a terminal job is a no-op.
- `WORKER_SECRET` from env; reject missing/wrong with 401. Only the **leader**
  can write completions (a follower can't write s3lite) — a follower returns a
  retryable 503, same as receive-pack.

**4b. Runner** (`cmd/gitmote-runner` + `Dockerfile.runner`):
- Read env; `GET` claim; `git clone https://x-access-token:$TOKEN@$GITMOTE_URL/$REPO`
  and checkout `$SHA` (shallow where possible).
- Run `act` over `.github/workflows`, capturing combined stdout/stderr.
- `POST …/complete` with pass/fail (from `act`'s exit) + the captured log. Exit
  non-zero on internal failure so a crashed runner is distinguishable from a
  failed build.
- `Dockerfile.runner`: `act` + git + the daemon strategy from the spike above.
  One-time provisioning (`scw jobs definition create name=gitmote-ci-runner
  image=… cpu-limit=… memory-limit=… local-storage-capacity=…`) documented in
  [ops.md](../docs/ops.md); one definition serves all repos (env injected at
  trigger, per stage 3).

**Reconciliation:** a runner that dies without completing leaves a `running` job
forever. Add a timeout sweep (the leader marks `running` jobs older than a bound
`error`); this pairs naturally with the log/run GC in stage 6/retention.

## Verify

- **internal API tests** (httptest, cf. pan0): claim transitions `queued`→
  `running` and returns the spec; **double-complete** is idempotent; missing/wrong
  `X-Worker-Secret` → 401 (constant-time); a follower (`IsLeader()==false`) →
  retryable 503; a log over 10 MiB is stored **truncated with the marker**, never
  silently cut; the log blob lands under `ci/{repoID}/{runID}/{jobID}.log` with
  the recorded `log_key`.
- **runner test** against a stub gitmote server with a **fake engine** (a script
  that echoes and exits 0/1): full claim→clone→run→complete flow drives the job
  to `passed`/`failed`; the reported log round-trips.
- **reconciliation:** a `running` job past the bound is swept to `error`.
- **security:** the runner holds only the scoped token + `WORKER_SECRET`; assert
  it has no S3/DB credentials in its env.
- `gofmt`/`golangci-lint`/`go test ./...` clean; `make e2e-local` +
  `make e2e-restore` green (internal CI routes don't touch the git write path).
