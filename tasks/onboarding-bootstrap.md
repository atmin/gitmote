# First-run auto-bootstrap + token recovery

Part of [easy to operate](README.md). A fresh empty-bucket `docker run` should be
usable without a second command, and a lost token must be recoverable without
nuking the bucket. Lands after [minimal-env-secrets](minimal-env-secrets.md) (the
cookie key must resolve for the UI).

## Spec

- **Auto-bootstrap on first run.** When this instance is the leader and **no admin
  exists**, create the admin (handle from `GITMOTE_ADMIN_HANDLE`, default
  `admin`), mint a token, and print it **once** to the logs behind an unmissable,
  self-explanatory banner: what it is, how to sign in (paste at `/login`), how to
  clone/push, that it won't be shown again, and how to re-mint if lost. No web
  setup page, no unauthenticated surface, no first-run race.
- **No initial repo.** Drop the bootstrap-creates-a-repo step; repos are made in
  the UI in two clicks. Removes the `-repo` / `-default-branch` inputs.
- **Recovery via `bootstrap --reissue`.** Mints a fresh token for the existing
  admin and prints it once — the safe answer to a lost token: whoever can run the
  container against the bucket already has total infra control, so out-of-band
  re-mint is the correct authz boundary. It needs the writer lease, so run it
  while the server is idle/stopped (scale-to-zero frees the lease). No token is
  ever stored at rest — meta keeps only the hash.

Why logs, not a page: the log is operator-only (same trust boundary as the env
and bucket), so a one-time token there is strictly simpler than a new HTTP
surface — no gating, no race, no claim-code machinery.

## Current

`gitmote bootstrap` is a separate CLI subcommand (needs `-handle`/`-repo`),
refuse-on-exists ([internal/bootstrap](../internal/bootstrap), `AdminExists()`
gates it), mints a token once. A fresh server is unusable until someone runs it;
a lost token means lock-out (re-bootstrap refuses) with only "nuke the bucket" as
an escape. `data/dev-token` is a local dev convenience, not a prod model.

## Change

- On server startup, after `meta.Open`, when leader && `!AdminExists()`: run
  `bootstrap.Run` and log the token with the banner. Subsequent boots: admin
  exists → skip. (A follower can't reach this — a first-ever boot is uncontended,
  so it is the leader.)
- Make the repo optional in `bootstrap.Run`; default the admin handle to `admin`.
- Add a `--reissue` flag (or mode) to the `bootstrap` CLI: with an admin present,
  mint a fresh token for it and print once (reuses the existing RoleWriter/lease
  machinery — no second subcommand).
- Update `scripts/dev.sh` to rely on auto-bootstrap (drop the manual bootstrap
  step + the `data/dev-token` dance; read the token from the server log or just
  re-issue).

## Verify

- Empty bucket: the first `docker run` logs a token + clear instructions; signing
  in at `/login` with it works.
- Second boot: no new token (admin exists); existing sessions/tokens still work.
- `bootstrap --reissue` mints a working fresh token for the existing admin without
  touching other data; requires the writer lease.
- Only the hash is stored (assert no raw token in meta/S3).
- Docs note: the token transits the logs (operator-visible) — rotate it after
  first login by minting your own and revoking `bootstrap`.
- `gofmt`/`golangci-lint`/`go test ./...` clean; golden + failure paths.
