# 07 — Write path: push (the CAS)

Depends on: 04, 03, 06.

## Spec

Serve `git-receive-pack` so `git push` works, with the safety-critical ordering
**objects durable in S3 first, ref CAS in s3lite second** — the write path and
all four points in [request-flows.md](../docs/architecture/request-flows.md) and
[safety.md](../docs/architecture/safety.md). Resolves the push-hook channel
([notes/push-hook-channel.md](../docs/notes/push-hook-channel.md)).

## Current

Read path only. No way to write.

## Change

- Routes: `GET /{repo}/info/refs?service=git-receive-pack` and
  `POST /{repo}/git-receive-pack`, gated by `write` ACL (task 06).
- A per-repo in-process mutex serializes writes; materialize target history
  (full-hydrate) before spawning `receive-pack` and piping the request body.
- Install a `pre-receive` hook into the materialized repo that RPCs the parent
  over a unix socket — **decide:** socket path passed via the hook's env, hook
  authenticated by a per-request nonce. The parent (the sole writer) PUTs
  quarantined objects to S3 (`$GIT_QUARANTINE_PATH`) — content before pointer —
  then runs the per-ref CAS in one s3lite transaction and returns a verdict the
  hook turns into its exit code.
- On `ok`, receive-pack migrates the quarantine; report-status flows back.

## Verify

- Golden: a real `git push` of new commits succeeds; a subsequent clone
  (task 05) returns them.
- Safety tests — the point of this task:
  - **non-fast-forward is rejected** and leaves refs unchanged.
  - **concurrent-push CAS**: two pushes racing the same ref → exactly one wins,
    the other is rejected; no lost update.
  - **atomic multi-ref**: a push updating two refs where one fails rolls back
    both.
  - **ordering invariant**: inject a ref-CAS failure after the object PUT → S3
    holds orphan objects (harmless), the ref never advanced (no corruption).
- Non-breaking: adds write endpoints; the read path is unaffected.
