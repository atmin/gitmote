# Concurrency & safety model

> Part of the [gitmote architecture](README.md).

**"Safe" is a hard requirement.** The model rests on four points:

1. **Single writer = single _container_, not single _user_.** s3lite requires
   exactly one writer instance; that's a deployment fact, not a limit on
   collaborators. A few invited users and the web UI's service commits all funnel
   into one container. This suits the scale-to-zero shape: 0↔1 instances is fine.
   **The one operational rule: never run two writer instances** — no overlapping
   deploys of the writer. A rolling deploy would violate this (it briefly runs
   old + new), so the deploy is **stop-first**: it drains the running writer to
   exit before starting the replacement (see [ops.md](../ops.md) — deploy). The
   airtight version — read replicas plus a single leased writer, so overlap is
   safe by construction — is the planned evolution
   ([reader-writer-split](../evolution/reader-writer-split.md)); the current
   drain closes the window in practice, not by proof.

2. **In-process mutex linearizes writes.** Git's own lockfiles can't be trusted
   across a synced/object backend, so a per-repo mutex in the process does the
   linearization. At "a few users," contention is ~zero. (This is the
   "in-process now, shared coordination later" pattern — the mutex is the
   linearization point today; the SQL CAS below keeps the door open to relax the
   single-writer assumption later without changing the storage contract.)

3. **The ordering invariant — content before pointer.** Objects are made durable
   in S3 _before_ the ref CAS in s3lite. Get this right and the only failure mode
   is unreferenced objects in S3 — harmless garbage that `gc` reclaims. Get it
   backwards and you can get a ref pointing at a missing object, the one true
   corruption. So: **always PUT objects, then advance the ref.**

4. **s3lite's write-loss window is accepted, and it's benign for git.** litestream
   replicates the SQLite WAL to S3 _asynchronously_ — a committed ref update can
   be lost if the container dies within a sub-second window. Because of the
   ordering above, the loss direction is always objects-without-a-ref (garbage),
   **never** ref-without-an-object (corruption). To the user it's a lost
   _acknowledgment_: the client still holds its commits, re-push is cheap (objects
   already uploaded), and `gc` sweeps the orphans. For git — content-addressed and
   idempotent — this is self-healing in a way a ledger isn't, so we accept it.
   (Escape hatch if ever needed: move _refs_ to plain S3 behind a synchronous
   conditional-PUT CAS, keeping only softer metadata in s3lite.)
