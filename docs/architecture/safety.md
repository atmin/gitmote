# Concurrency & safety model

> Part of the [gitmote architecture](README.md).

**"Safe" is a hard requirement.** The model rests on these points:

1. **Single writer = single _lease-holder_, not single _user_.** s3lite requires
   exactly one writer instance; that's a deployment fact, not a limit on
   collaborators. A few invited users and the web UI's service commits all funnel
   into one writer. **The one rule: never two writers on the same WAL** — and this
   is enforced by a **lease**, not by procedure. The server opens s3lite in
   `RoleAuto`: it writes only while it holds the lease (litestream's `s3.Leaser`,
   a conditional-write CAS on `lock.json` in the replica bucket) and otherwise runs
   as a read-only follower. This makes the S3 backend's support for **atomic
   conditional writes** (compare-and-swap: `If-None-Match`/`If-Match`) a correctness
   requirement — the lease can't be enforced without it, so a provider lacking the
   primitive cannot safely back gitmote. gitmote gates **all metadata-derived requests** on
   `IsLeader()` — a follower refuses them with a retryable `503`; only the liveness
   probes and static assets stay up (gating those would deadlock a rolling deploy,
   since the new instance can't promote until the old drains). Writes are gated for
   *safety*, reads for *freshness*: a follower serves only the snapshot it restored
   at startup and doesn't catch up until it promotes, so serving a read would return
   a stale ref — a just-pushed file appearing missing, a `fetch` missing commits.
   A rolling deploy's
   brief old+new overlap is therefore one writer + one follower: the new instance
   boots as a follower, the old releases the lease on its graceful SIGTERM `Close`,
   and the new promotes on its next lease poll. **Overlap is safe by construction**,
   so there is no stop-first drain. This is the writer half of
   [reader-writer-split](../evolution/reader-writer-split.md), shipped; the
   remaining half — *fresh* followers that scale reads out (a follower serves only
   a restored snapshot today) — is why the container still pins `max-scale = 1`.

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

5. **CI secrets are encrypted at rest, and the threat model is narrow — say so.**
   Per-repo CI secrets are sealed with AES-256-GCM under a **server-held** master
   key (`GITMOTE_CI_SECRET_KEY_V<n>`, a container env var alongside the AWS keys —
   env-only, never persisted; see §6); a per-repo subkey is derived with
   HKDF-SHA256, and the envelope's AAD binds `(repoID, name, version)` so a stolen
   ciphertext can't be replayed under a different repo/name/version. Only the
   sealed `{v, iv, ct}` lives in s3lite — never the plaintext or the key. So the
   one property this buys is: **a compromise of the S3 replica / DB snapshot (the
   litestream WAL, the object bucket) does not leak secret values.** It is
   explicitly **not** a defense against a compromised *running* server, which
   holds the master key by necessity — it must decrypt to inject secrets into the
   runner env at trigger. There is no user-password KDF here (unlike an E2E
   design): the server is the trust boundary. Rotation is a new key version + bump
   (old envelopes still decrypt under their own version); values are write-only in
   the UI (only names are ever shown) and are never logged.

6. **Two server secrets are auto-provisioned into the replica — the CI master key
   is not.** The session cookie key (`GITMOTE_COOKIE_KEY`) and the CI worker
   secret (`WORKER_SECRET`) are generated on first boot and persisted in
   `server_secrets` when no env overrides them, so a scale-to-zero container keeps
   sessions valid and in-flight CI authenticated across an idle→wake (a per-boot
   key would log everyone out and orphan runs). This is acceptable precisely
   because the replica already holds users, tokens, and ACLs; sessions are
   short-lived; and the worker secret only authorizes CI report submission, never
   decryption. The CI **master key** stays env-only by contrast (§5): it decrypts
   every repo's secrets, so a replica leak must never expose it. An explicit env
   still wins for both, and generation is get-or-create on the leader (a follower's
   DB is read-only), so restarts and rolling deploys reuse the one restored value.

7. **Container image builds are an opt-in privilege escalation — off by default.**
   Workflow code is attacker-controlled; the isolation boundary is the job
   container. Building an image needs a container builder, and the pragmatic path
   (`act` nested mode) mounts the **host Docker socket** into the job container —
   which is equivalent to handing that untrusted code **root on the host** (a
   trivial container escape). So gitmote's default **suppresses** the mount
   (`act --container-daemon-socket -`, in [`internal/runner/act.go`](../../internal/runner/act.go)),
   and only restores it when **`GITMOTE_CI_ALLOW_BUILDS`** is set. This is
   deliberately not per-repo: it is a host-level trust decision the operator makes
   for a machine running only repos they trust (a personal/VPS forge building its
   own code). It has no effect on the Scaleway substrate (self-hosted mode, no
   daemon, no socket to mount — and it can't build anyway). The server logs a loud
   warning at startup when the opt-in is on.

   **Why not just always use Docker-in-Docker (DinD) and drop the knob?** Because
   DinD does not remove the escape — it relocates it. A nested daemon requires a
   `--privileged` container (all capabilities, host devices, unmasked `/proc`/`/sys`,
   no seccomp), which is itself host-root-equivalent; and untrusted workflow code is
   exactly what drives the builder, so it inherits that reach. DooD vs DinD is a
   choice about build *state isolation* (own daemon/cache vs the host's), **not**
   about safety against untrusted code. The axis that actually bounds untrusted
   builds is **rootless vs rootful**: on a rootful daemon anything the build reaches
   is real host root, whereas a rootless engine caps the whole container tree —
   privileged nesting included — at the invoking unprivileged user. So "always DinD
   where supported" would be *more* machinery (provision/wire/lifecycle a privileged
   daemon, detect support — and Scaleway can't do privileged, leaving the same
   local/VPS set), still couldn't drop the operator-consent gate (auto-enabling
   builds for any pushed repo is a host-root grant by default), and buys no safety.
   A genuine "solve it from the root" is a **rootless builder** (rootless
   BuildKit/kaniko/buildah — an escape lands as an unprivileged user) or a
   **per-job VM boundary** (Kata/Firecracker); both are additions, tracked as
   future work, and the consent gate stays until one exists.
