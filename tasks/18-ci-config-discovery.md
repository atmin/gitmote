# CI stage 2 — workflow config discovery

Part of the CI epic ([16-ci.md](16-ci.md)). Depends on **stage 1
([17](17-ci-data-model.md))** having landed: `ci_runs`/`ci_jobs` tables, the
`ci.Dispatcher`, and the after-commit seam. Engine is locked to **`act`**, so the
workflow location/format is `.github/workflows/*.yml|*.yaml`, GitHub Actions YAML
(epic §Stage 0). Still no execution — this stage decides *whether* there is work
and records the jobs.

## Spec

When the dispatcher fires for a branch push, read the pushed commit's
`.github/workflows/` directory at the new SHA:

- **No workflow files** → create **no run** (nothing to do; log at debug).
- **Workflow file(s) present** → the run gets one `ci_job` per workflow file.
- **Malformed workflow** (not valid YAML) → the run is recorded **failed** with a
  clear message, never a crash or panic in the write path.

Deep validation (does `act` accept it, do the jobs make sense) is the runner's
job at execution time; this stage only does cheap syntactic gating.

## Current

- Stage 1's `ci.Dispatcher.Dispatch` creates a `queued` run for a branch update
  and stops there.
- The browse reader ([internal/repo/browse.go](../internal/repo/browse.go)) reads
  a tree/blob at an arbitrary SHA over a materialized dir: `repo.Tree(ctx, dir,
  sha, path)` and `repo.Blob(ctx, dir, sha, path)`, with `safePath` already
  rejecting traversal. `ErrNotFound` distinguishes "no such path".
- The shared `*repo.Materializer` (built in [main.go](../cmd/gitmote/main.go),
  passed to githttp and webui) turns a repo name into an on-disk dir via
  `Materialize(ctx, name)` — idempotent and warm right after a push.
- No YAML dependency in [go.mod](../go.mod) yet.

## Change

**1. Give the `Dispatcher` the `Materializer`** (constructor dep). After
`CreateRun`, `Materialize(repoName)` to get the dir (the push just materialized
it, so this is warm), then `repo.Tree(ctx, dir, sha, ".github/workflows")`.

- `repo.ErrNotFound` (dir absent) → treat as "no workflows": delete/skip the run
  (prefer **not creating the run until after** discovery, so an empty result
  leaves no row — reorder stage 1's `CreateRun` to follow discovery).
- Filter entries to `type == blob` with a `.yml`/`.yaml` suffix.

**2. Minimal YAML validation** — add `gopkg.in/yaml.v3`; for each workflow blob,
`repo.Blob` then `yaml.Unmarshal` into a loose `map[string]any`. Syntactically
valid → create a `ci_job` (name = the file's base name) in `queued`. Any file
failing to parse → set the **run** `failed` with the first parse error as the
message and still create the job rows you can, so the failure is visible in the
UI (stage 6). No file parses / none present → no run.

**3. `meta` job helpers** — `CreateJob(ctx, runID, name)`, `ListJobs(ctx,
runID)`, `SetJobStatus`. (Run status setter exists from stage 1.)

Keep discovery **non-fatal** to the push, exactly like stage 1: a materialize or
read error marks the run `error` and is logged; it never propagates to the client.

## Verify

- **dispatcher unit tests** (with a seeded materialized fixture repo, cf.
  [webui/browse_test.go](../internal/webui/browse_test.go) `seedBrowseRepo`):
  - repo with one workflow → run + one `queued` job named after the file.
  - repo with **no** `.github/workflows` → **no run** created.
  - two workflow files → two jobs under one run.
  - a **malformed** workflow → run recorded `failed` with the parse message, no
    panic; the push (simulated event) is unaffected.
- **path safety:** the fixed `.github/workflows` path plus `safePath` means no
  traversal; add a test that a repo without the dir 404s cleanly rather than
  erroring the run as a crash.
- `gofmt`/`golangci-lint`/`go test ./...` clean; e2e stays green.
