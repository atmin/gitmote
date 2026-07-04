# Auto-generate and persist COOKIE_KEY and WORKER_SECRET

Part of [easy to operate](README.md). Make the UI and local CI work with **zero
secret config**, stable across scale-to-zero.

## Spec

`GITMOTE_COOKIE_KEY` (signs session cookies) and `WORKER_SECRET` (authenticates
the CI report API) should not be required. Generate each on demand, **persist in
meta** (survives restart / scale-to-zero via S3 restore), reuse thereafter. An
env, when set, wins. The CI-secrets master key (`GITMOTE_CI_SECRET_KEY_V<n>`)
stays **env-only** — it must not live in the replica (safety.md §5); unchanged.

Persist, not ephemeral: a per-boot key would log everyone out on each idle→wake
and orphan in-flight CI — wrong for a scale-to-zero container.

Scope tightly: this is get-or-create for **exactly these two named keys**, not a
general key–value abstraction (CLAUDE.md §3 — no speculative generality).
`WORKER_SECRET` is generated **lazily** — only when a CI trigger is configured
(it's meaningless with CI off), so a plain no-CI run generates only the cookie key.

## Current

`GITMOTE_COOKIE_KEY` absent → [`buildUI`](../cmd/gitmote/main.go) returns nil, the
UI is disabled. `WORKER_SECRET` absent → the CI trigger is `NoopTrigger` (or a
fail-fast when `SCW_CI_JOB_DEFINITION_ID` is set). Both are hand-supplied today.
The schema is `CREATE TABLE IF NOT EXISTS` + an additive `migrate()` (meta.go), so
adding a `server_secrets` table needs no migration infra.

## Change

- Add a `server_secrets(name TEXT PRIMARY KEY, value BLOB)` table and a narrow
  `GetOrCreateSecret(ctx, name, gen func() ([]byte, error))` in
  [`internal/meta`](../internal/meta), leader-written like any row.
- At startup resolve each: env → else meta get-or-create. The **leader** generates
  + persists on first boot (uncontended cold start is leader); later boots / a
  follower read the restored value.
- Wire the cookie key into `buildUI` (UI always on) and the worker secret into the
  dispatcher + report API — generating the worker secret only when a trigger is
  configured.
- Note in `safety.md`: these two now live in the replica (distinct from the
  env-only CI master key), and why it's acceptable (the replica already holds
  users/tokens/ACLs; sessions are short-lived; the worker secret only authorizes
  CI report submission).

## Verify

- UI works with **no** `GITMOTE_COOKIE_KEY`; a session stays valid across a
  process restart (same key restored), resetting only if meta/S3 is wiped.
- **Cloud propagation:** an auto-generated `WORKER_SECRET` reaches the Scaleway
  runner — it's injected at trigger time (`env["WORKER_SECRET"] = d.secret` in
  dispatcher.go), not baked into the job definition. Add a test asserting the
  resolved secret is the one the dispatcher injects and the report API validates.
- An explicit env overrides the persisted value.
- `gofmt`/`golangci-lint`/`go test ./...` clean; golden + failure paths.
