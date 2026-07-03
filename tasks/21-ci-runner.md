# CI stage 4b(cloud) — Scaleway runner image

Part of the CI epic ([16-ci.md](16-ci.md)). The runner and the whole execution
path have **landed and run locally**: on a push, gitmote records a run, mints a
scoped clone token, and triggers `gitmote-runner`, which claims the job, clones
at the SHA, runs `.github/workflows` with `act`, and reports the log + pass/fail.
What remains is packaging that exact runner to run on **Scaleway Serverless Jobs**
instead of a local process.

## What has landed (the whole local path)

- **Runner** — `cmd/gitmote-runner` + `internal/runner` (claim → clone@SHA →
  engine → complete). Engine is `act` (`internal/runner/act.go`); clone is the
  normal git-HTTP path with a scoped token (`git.go`). Substrate-agnostic: the
  same binary runs locally and (once this task lands) in a container.
- **Local trigger** — `ci.LocalTrigger` spawns the runner as a local process,
  the dev analog of Scaleway starting a job. `main.go` picks it when
  `GITMOTE_URL` + `WORKER_SECRET` are set and `SCW_CI_JOB_DEFINITION_ID` is not.
  `make dev` wires this, so **CI is testable locally** (needs `act` + Docker).
- **Real clone token** — the dispatcher mints a read-only, repo-scoped, expiring
  token under the pusher (`auth.Guard.MintScoped`), injected as
  `GITMOTE_CI_CLONE_TOKEN`. No CI service identity or ACL grant needed.
- **Report API + reconcile** — `internal/ci/report.go` (claim/complete, idempotent,
  content-before-pointer log store) plus a leader-only `ReconcileStuck` ticker in
  `main.go` that sweeps abandoned `running` jobs to `error`.

## The remaining work — cloud packaging

> **Infra spike to resolve first:** `act` runs each job in a Docker container, so
> the runner needs a Docker daemon inside the Scaleway Serverless Job. Confirm
> Jobs expose a usable daemon (privileged / DinD) or pick the fallback: run steps
> in a single provided container without full `act` nesting. Spike this before
> building the image. (Local dev sidesteps this — the host daemon is real.)

- **`Dockerfile.runner`** — `act` + git + the `gitmote-runner` binary + the daemon
  strategy from the spike. The runner reads the same env it does locally.
- **`scw jobs definition create`** — one-time, out of band:
  `name=gitmote-ci-runner image=… cpu-limit=… memory-limit=…
  local-storage-capacity=…`, documented in [ops.md](../docs/ops.md). One
  definition serves all repos (env injected at trigger, per stage 3). Setting
  `SCW_CI_JOB_DEFINITION_ID` flips `main.go` from the local trigger to Scaleway —
  no code change.

## Verify

- **image smoke:** a container built from `Dockerfile.runner`, given a job's env
  against a reachable gitmote, drives a real workflow to `passed`/`failed`; the
  log round-trips (the local path already proves the runner logic).
- **security:** the runner env holds only the scoped clone token + `WORKER_SECRET`
  — no S3/DB credentials (already true of the env contract; assert it in the job
  definition).
- `gofmt`/`golangci-lint`/`go test ./...` clean; `make e2e-local` +
  `make e2e-restore` green.
