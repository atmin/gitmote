# CI stage 4 (precursor) — scoped, expiring access tokens

Part of the CI epic ([16-ci.md](16-ci.md), decision §Stage 0 #4). A small,
self-contained extension to the token model, valuable on its own (expiring scoped
tokens are generally useful), and the prerequisite for the runner's **per-run,
read-only, repo-scoped clone credential** in stage 4 ([21](21-ci-runner.md)). No
CI wiring here — just the auth capability + tests.

## Spec

A personal access token can carry three new, optional constraints, all enforced
at auth time:

- **expiry** — past `expires_at` → the token is rejected.
- **repo scope** — a scoped token authorizes only its one repo; any other repo is
  denied even if the owner has ACLs there.
- **read-only** — a read-only token is denied for write/admin operations
  (push), allowed for read (clone/fetch).

Existing unscoped PATs are unaffected: absent = no expiry, all the owner's repos,
not read-only.

## Current

- The `tokens` table ([schema.sql](../internal/meta/schema.sql#L28)) has
  `id, user_id, selector, verifier, label, created_at, last_used` — **no** expiry
  or scope.
- [`auth.Guard.VerifyToken`](../internal/auth/guard.go#L72) looks the row up by
  selector (`meta.TokenBySelector`), compares the verifier in constant time, and
  returns the owner; [`Authorize`](../internal/auth/guard.go#L31) then checks the
  ACL perm via `allows`. Neither consults expiry or scope.
- `Mint()` ([auth/token.go](../internal/auth/token.go#L42)) makes the
  `selector.secret` pair; `meta.CreateToken` persists it.
- The schema is applied on every `Open` and **must stay idempotent**, and s3lite
  has **no version table**. SQLite has no `ADD COLUMN IF NOT EXISTS`, so a bare
  `ALTER TABLE … ADD COLUMN` would fail on the second `Open` of an existing DB —
  this is the one fiddly part.

## Change

**1. Schema migration (idempotent, mind the existing prod DB).** Add nullable
columns `expires_at TEXT`, `repo_scope INTEGER REFERENCES repos(id)`, and
`read_only INTEGER NOT NULL DEFAULT 0` to `tokens`. Because the columns must be
added to an already-populated table without a version table, do it as a small
**guarded Go migration** in [meta.go](../internal/meta/meta.go): query
`pragma_table_info('tokens')` and `ALTER TABLE tokens ADD COLUMN …` only for
columns not already present. Fresh DBs can get them straight in the `CREATE TABLE`
for clarity; the guarded ALTER covers the existing prod DB. (Document this as the
established pattern for future additive columns.)

**2. `meta`** — `TokenBySelector` returns the new fields on the token record; add
`CreateScopedToken(ctx, userID, selector, verifier, label string, repoScope
*int64, readOnly bool, expiresAt time.Time) (Token, error)`. `DeleteToken`
(revoke) already exists.

**3. `auth.Guard`** — inject a clock (`now func() time.Time`, default
`time.Now`, override in tests):
- `VerifyToken`: after the verifier check, reject with `ErrUnauthorized` when
  `expires_at` is set and in the past. Optionally bump `last_used`.
- `Authorize`: if the token is `repo_scope`d and the requested `repoName`'s id ≠
  the scope → deny; if `read_only` and the required perm is write/admin → deny.
  This means `Authorize` must thread the token's constraints from verification
  into the perm decision (return them from a shared internal `authenticate` step
  rather than re-fetching).
- Add `MintScoped(...)` helper (or expose via the guard) that mints + persists a
  scoped token and returns the raw string, for stage 4's dispatcher to call.

## Verify

- **auth unit tests** (golden + failure, cf. existing guard tests):
  - a not-yet-expired token authorizes; an **expired** one is rejected (inject a
    clock).
  - a token scoped to repo A authorizes A, is **denied** on repo B (even with an
    ACL on B).
  - a **read-only** token allows a read perm, is **denied** a write perm.
  - an ordinary unscoped PAT is unaffected (no expiry, any owned repo, read+write
    per ACL).
- **migration idempotency:** open an existing DB (with a pre-populated `tokens`
  table lacking the columns) twice — no error, columns present, old rows read
  back with NULL/0 defaults. A fresh DB works too.
- `gofmt`/`golangci-lint`/`go test ./...` clean; `make e2e-local` +
  `make e2e-restore` green (existing push/clone auth unchanged).
