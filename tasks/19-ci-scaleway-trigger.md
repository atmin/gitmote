# CI stage 3 — Scaleway Serverless Jobs client + trigger

Part of the CI epic ([16-ci.md](16-ci.md)). Depends on **stages 1–2
([17](17-ci-data-model.md), [18](18-ci-config-discovery.md))**: the dispatcher
now creates a run with `queued` jobs. This stage makes the dispatcher **trigger a
Scaleway Serverless Job per CI job** — fire-and-forget. The runner binary and the
report-back API are stage 4 ([21](21-ci-runner.md)); this stage only fires the
trigger and records the outcome.

## Spec

For each `queued` job, POST a Scaleway Serverless Jobs `start` with per-run env
(the runner reads it in stage 4). The trigger is fire-and-forget: a failure marks
that job/run `error` and is logged, but never blocks the push or the other jobs.
When the job-definition ID is unset (local dev / tests), the trigger is a
**no-op** so the whole pipeline runs under a stub.

## Current

- The dispatcher creates runs + jobs (stages 1–2) but does nothing external.
- **Proven, stealable pattern** in the sibling project
  `~/dev/pan0.com/lib/scaleway/jobs.go`:
  `POST https://api.scaleway.com/serverless-jobs/v1alpha2/regions/{region}/job-definitions/{id}/start`,
  header `X-Auth-Token: <SCW_SECRET_KEY>`, body
  `{"environment_variables": { … }}`; a `10s` client timeout on the trigger call
  only; a no-op when the definition ID is empty. Its `server.go` shows the env
  wiring and the "fail fast if the definition ID is set but required env is
  missing" check.
- gitmote's public base URL isn't currently a config value; the container knows
  it as `gitmote.atmin.net` (see [ops.md](../docs/ops.md)).

## Change

**1. `internal/scaleway/jobs.go`** — port pan0's client, adapted:

```go
type JobsClient struct {
    secretKey, region, jobDefinitionID string
    httpClient *http.Client
}
func NewJobsClient(secretKey, region, jobDefinitionID string) *JobsClient { … }

// Trigger starts one job run with the given env. No-op when jobDefinitionID == "".
func (c *JobsClient) Trigger(ctx context.Context, env map[string]string) error
```

`Trigger` marshals `{"environment_variables": env}`, sets `X-Auth-Token`, POSTs to
the `…/start` URL, and treats any `>= 300` as an error (include the response body).

**2. Dispatcher wiring.** After jobs are created (stage 2), for each job call
`Trigger` with env:

| env | value |
|---|---|
| `GITMOTE_CI_RUN_ID` / `GITMOTE_CI_JOB_ID` | the run/job ids |
| `GITMOTE_URL` | gitmote's public base URL (new config) |
| `WORKER_SECRET` | shared runner-auth secret (used in stage 4) |
| `GITMOTE_REPO` / `GITMOTE_SHA` / `GITMOTE_REF` | what to check out |
| `GITMOTE_CI_CLONE_TOKEN` | per-run scoped token — **placeholder in this stage**, wired once [20](20-ci-scoped-tokens.md) lands |

A `Trigger` error → `SetJobStatus(error)` (and the run `error` if all jobs fail);
log and continue. Still non-fatal to the push.

**3. Config/env plumbing** ([main.go](../cmd/gitmote/main.go)): read
`SCW_SECRET_KEY`, `SCW_REGION` (fallback `AWS_REGION`), `SCW_CI_JOB_DEFINITION_ID`,
`WORKER_SECRET`, `GITMOTE_URL`; build the `JobsClient` and pass it to the
`Dispatcher`. When `SCW_CI_JOB_DEFINITION_ID` is unset, the dispatcher still
creates run/job rows but the trigger no-ops (local dev behaves fully except for
the external call). Add these to [ops.md](../docs/ops.md)'s env tables (the
one-time `scw jobs definition create` for the runner image lands with stage 4).

## Verify

- **`scaleway` unit tests** with an `httptest.Server`: `Trigger` POSTs the correct
  URL path (region + definition id), sets `X-Auth-Token`, and sends
  `{"environment_variables": …}` with the expected keys; a `>= 300` response is a
  returned error including the body.
- **no-op:** `NewJobsClient("","","")`.Trigger returns nil without any HTTP call.
- **dispatcher tests:** with a stub trigger, a successful dispatch leaves jobs
  `queued`→(triggered) and the run intact; a trigger that errors marks the job
  `error` and the push/event still succeeds; one job's trigger failure doesn't
  abort the others.
- `gofmt`/`golangci-lint`/`go test ./...` clean; e2e green (the trigger no-ops in
  the e2e env, so the write path is unchanged).
