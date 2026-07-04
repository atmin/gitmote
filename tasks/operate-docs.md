# Operate docs — quickstart + CI substrate

Part of [easy to operate](README.md). Lands after the env cuts + GHCR, so the docs
describe the reduced surface.

## Spec

A newcomer copies one `docker run` and has a working forge. Make true in the docs:

1. **Run quickstart** — the minimal env set (`GITMOTE_S3_BUCKET`, the three
   `AWS_*`, `GITMOTE_S3_ENDPOINT` if non-AWS; optionally `GITMOTE_DATA` for a
   mounted volume) and a copy-paste `docker run ghcr.io/atmin/gitmote`. State that
   S3 is the single source of truth and the container is disposable
   (kill/restart restores), and that **the first-run admin token is printed once
   to the logs** — where to look, how to sign in, and how to re-mint
   (`bootstrap --reissue`) if lost.
2. **CI substrate auto-detection** — how gitmote picks its runner: present
   `SCW_CI_JOB_DEFINITION_ID` → Scaleway Serverless Jobs; else
   `GITMOTE_URL`+`WORKER_SECRET` → local `act` (needs docker/podman on the host);
   else CI records runs but does not execute. State the docker/podman prerequisite
   explicitly.

## Current

`docs/ops.md` documents the full pre-cut env list and the Scaleway-first deploy;
no minimal quickstart, and the CI substrate fallback is undocumented (it's in
[ci.md](../docs/architecture/ci.md)'s runner table but not as operator guidance).
`README.md` has no "run it" path.

## Change

- Add a **Quickstart** to `README.md` (or a short `docs/run.md`): the minimal env
  set, `docker run ghcr.io/atmin/gitmote …`, and the "grab the token from the
  logs" first-login step.
- Refresh `docs/ops.md`'s env table: mark removed (`GITMOTE_DB_REPLICA`, the
  individual `GITMOTE_DB`/`CACHE`/`SOCK` → `GITMOTE_DATA`), auto-managed
  (`GITMOTE_COOKIE_KEY`, `WORKER_SECRET`), and feature-gated (`SCW_*`,
  `GITMOTE_URL`, `GITMOTE_CI_SECRET_KEY_V<n>`); keep the required core small.
- Add a short **CI substrate** section (auto-detection rules + docker/podman note)
  and cross-link `ci.md`.

## Verify

- The env tables match the code after the earlier tasks (no stale vars).
- Following the quickstart from a clean machine yields a running, pushable forge.
- Docs and code agree (CONTRIBUTING's "fix one in the same change").
