# CI stage 1 — run/job data model + after-commit dispatch seam

Part of the CI epic ([16-ci.md](16-ci.md)); rationale + locked decisions live
there and in [../docs/evolution/ci-runner.md](../docs/evolution/ci-runner.md).
This stage adds **no execution** — it makes "a branch advanced" durably enqueue a
CI run, and nothing more. Non-breaking on its own.

## Spec

On a successful push that advances a branch, record a **queued CI run** in the
metadata DB. The dispatch is **fire-and-forget**: it must never block or fail the
push (a missed run is not a failed push — the content-before-pointer discipline,
[safety.md §3](../docs/architecture/safety.md), applied to CI). Only the leader
dispatches (it is the only instance that processes receive-pack).

## Current

- The one durable "a ref advanced" moment is the ref CAS in
  [`Writer.handle`](../internal/githttp/receive.go#L189): after
  `w.meta.CASRefs(...)` succeeds it returns `Response{OK:true}`. There is already
  a `beforeCAS func() error` hook field on `Writer` (used before the CAS); there
  is **no after-commit hook**.
- `Writer` is built by [`NewWriter`](../internal/githttp/receive.go#L60) and wired
  in [cmd/gitmote/main.go](../cmd/gitmote/main.go#L210).
- The schema ([internal/meta/schema.sql](../internal/meta/schema.sql)) is applied
  on every `Open` and must stay idempotent (`CREATE TABLE IF NOT EXISTS`; s3lite
  has no version table). No CI tables exist.
- `meta` write helpers (e.g. [refs.go](../internal/meta/refs.go),
  [repos.go](../internal/meta/repos.go)) are the pattern for new CRUD.

## Change

**1. Schema** ([schema.sql](../internal/meta/schema.sql)) — two idempotent tables:

```sql
CREATE TABLE IF NOT EXISTS ci_runs (
  id         INTEGER PRIMARY KEY,
  repo_id    INTEGER NOT NULL REFERENCES repos(id),
  ref        TEXT NOT NULL,                    -- "refs/heads/main"
  sha        TEXT NOT NULL,                    -- the new tip
  status     TEXT NOT NULL,                    -- queued|running|passed|failed|error|superseded
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS ci_jobs (
  id          INTEGER PRIMARY KEY,
  run_id      INTEGER NOT NULL REFERENCES ci_runs(id),
  name        TEXT NOT NULL,                   -- workflow file / job name (filled in stage 2)
  status      TEXT NOT NULL,                   -- queued|running|passed|failed|error
  log_key     TEXT,                            -- ci/ object key, set on completion (stage 4)
  created_at  TEXT NOT NULL,
  updated_at  TEXT NOT NULL
);
```

**2. `meta/ci.go`** — CRUD, written **only by the leader**: `CreateRun(ctx,
repoID, ref, sha) (Run, error)` (status `queued`), `SetRunStatus`, `GetRun`,
`ListRuns(ctx, repoID, limit)`; job helpers arrive in later stages (stage 2
creates job rows). Mirror the existing `now()`/`parseTime` conventions.

**3. A `ci` package with a `Dispatcher`** (`internal/ci`) so the write path stays
thin and this is unit-testable. Stage 1's `Dispatcher.Dispatch(ctx, ev)` takes a
small event `{RepoID, RepoName, Ref, OldSHA, NewSHA}` and, **only for a branch
create/update** (`refs/heads/*`, new ≠ zero), calls `meta.CreateRun`. Tag pushes
and deletions create no run. Later stages extend `Dispatch` (config read, trigger).

**4. The after-commit seam** — add an `AfterCommit func(context.Context,
[]CommitInfo)` field to `Writer`, invoked once **after** `CASRefs` succeeds,
before returning `OK`. It must be non-fatal: wrap it in its own bounded context
and **recover-and-log** any panic/error — the push has already committed and must
return green regardless. `CommitInfo` carries `{Ref, Old, New}` per updated ref;
the handler already has `op.repoID`/`op.repoName` and `req.Commands`. Wire it in
[main.go](../cmd/gitmote/main.go) to `ci.Dispatcher.Dispatch`.

## Verify

- **meta unit tests:** `CreateRun` inserts a `queued` row with the right
  repo/ref/sha; `SetRunStatus` transitions; `ListRuns` orders newest-first.
- **dispatcher unit tests:** a branch update → one run; a tag push → none; a
  branch **delete** (new = zero) → none; a multi-ref push → one run per updated
  branch.
- **seam integration test** (githttp, real `git push` like
  [receive_test.go](../internal/githttp/receive_test.go)): a fast-forward push
  produces exactly one `queued` run for the branch tip; a **rejected**
  non-fast-forward push produces **none**; an `AfterCommit` that panics or errors
  leaves the push **successful** (the client sees a green push, the ref is
  advanced) — this is the safety invariant and gets an explicit test.
- `gofmt`/`golangci-lint`/`go test ./...` clean; `make e2e-local` +
  `make e2e-restore` stay green (the write path's success contract is unchanged).
