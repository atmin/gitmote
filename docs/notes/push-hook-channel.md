# The push hook channel

The control channel between git's `pre-receive` hook and the gitmote parent
process — plus how the hook gets wired into materialized repos.

**Context.** Push durability gates _inside_ `receive-pack`, at the `pre-receive`
hook (see [ARCHITECTURE.md](../../ARCHITECTURE.md) → "Push (write path)"). The
hook runs as a separate process and cannot touch the embedded single-writer
s3lite, so it RPCs back to the parent — the sole writer — which performs the S3
PUT + ref CAS and returns a verdict the hook turns into its exit code.

Three things that channel needs, none yet pinned down.

## 1. Socket discovery

How the hook finds the parent's unix socket.

- **Leaning:** the parent exports the socket path into `receive-pack`'s
  environment; the hook reads `$GITMOTE_SOCK`. (A fixed well-known path works
  too, but env is explicit and testable.)

## 2. Authentication

A random local process must not be able to forge a ref update.

- **Leaning:** the parent mints a **single-use nonce** per push, bound to the
  in-flight operation (repo, user, lock holder), and passes it into
  `receive-pack`'s environment. The hook presents the nonce on the socket; the
  parent validates it against the live push and burns it. It expires with the
  push.

## 3. Hook installation

Ephemeral / materialized repos must have the hook present without reinstalling
per repo.

- **Leaning:** set git's `core.hooksPath` (container-global config) to a fixed
  path holding the gitmote hook binary; every materialized repo inherits it. The
  binary is tiny — a socket client that forwards stdin + `$GIT_QUARANTINE_PATH` +
  the nonce and relays the verdict.

## Payload sketch

- Hook → parent: `{ nonce, repo, ref_commands (from stdin), quarantine_path }`.
- Parent → hook: `ok` (hook exits 0) or `reject: <reason>` (hook exits ≠ 0,
  reason to stderr → surfaced to the client).

## Open sub-points

- Socket lifecycle — one per container, created at boot.
- Exactly what binds the nonce to a push (the per-repo lock token? a request id?).
- Timeout / parent-crash handling on the hook side — fail closed (reject).
