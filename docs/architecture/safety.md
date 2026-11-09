# Concurrency and safety model

> Part of the [gitmote architecture](README.md).

Safety is a hard requirement. The model rests on these seven points.

## 1. A single writer holds the lease

A single writer means a single lease holder, not a single user. s3lite requires
exactly one writer instance. That is a fact about the deployment, not a limit on
collaborators. A few invited users and the service commits of the web UI all go
through one writer.

The one rule is that two writers never write to the same WAL. A lease enforces
this rule, not a procedure. The server opens s3lite in `RoleAuto`. It writes only
while it holds the lease, and runs as a read-only follower otherwise. The lease is
litestream's `s3.Leaser`, a compare-and-swap (CAS) by conditional write. It works
on `lock.json` beside the replica, at `{root}/meta/lock.json`.

So the S3 backend must support atomic conditional writes: a CAS with
`If-None-Match` and `If-Match`. This is a correctness requirement, because gitmote
cannot enforce the lease without it. A provider that lacks this primitive cannot
safely back gitmote.

### Writes need the lease, and reads do not

gitmote gates every request that may write metadata on the writer lease. These
requests are writes:

- a push: the `git-receive-pack` advertisement and POST
- a POST of the web UI, except sign-in and sign-out
- any request under `/internal/ci/`, because the job claim is a `GET` that writes

A follower promotes on demand with `s3lite.TryPromote`, which uses the same CAS.
Until it can promote, the follower refuses a write with a retryable `503`.

A read never takes the lease. A follower first catches up with `s3lite.Refresh`,
and then serves the read. So a read on a follower lags the writer by at most the
window of §4. When the catch-up fails, the read gets the retryable `503`. On the
writer, the catch-up does nothing. A follower never writes for a read: a token
check skips the `last_used` stamp there.

The liveness probes and static assets need neither the lease nor the catch-up.
Gating them would deadlock a rolling deploy, because the new instance cannot
promote until the old one drains.

### Two instances take turns

A writer keeps the lease only while it is busy. When no request is in flight and
none has ended for 5 s, the writer yields the lease. The yield
(`s3lite.YieldLease`) runs a final sync before it releases the lease. Followers
promote only on demand (`OnDemandPromotion`). So two long-lived instances on one
storage root take turns as the writer, and never overlap. An example is an
always-on local container beside the cloud instance.

The metadata can change under an instance while it is a follower. So the repo
cache rewrites its refs from the metadata DB before every request (see
[storage](storage.md)).

### Rolling deploys

A rolling deploy runs the old and the new instance together for a short time. This
overlap is also one writer and one follower:

1. The new instance boots as a follower.
2. On a graceful SIGTERM, the old instance releases the lease in `Close`.
3. The new instance promotes on demand when the first write arrives.

The overlap is safe by construction, so gitmote needs no stop-first drain.

The container pins `max-scale = 1`. A follower serves reads, but it refuses every
push. Nothing routes a push to the writer behind one URL.

## 2. An in-process mutex linearizes writes

gitmote cannot trust git's own lockfiles across a synced or object backend. So a
per-repo mutex in the process linearizes the writes. At the scale of "a few
users", contention is about zero.

The mutex is the linearization point inside the one writer. The ref CAS in SQL
(§3) does not depend on the mutex, so the storage contract holds without it.

## 3. Content comes before the pointer

This is the ordering invariant. Objects become durable in S3 before the ref CAS in
s3lite. With this order, the only failure mode is unreferenced objects in S3. They
are harmless garbage. gitmote never deletes them.

With the reverse order, a ref can point at a missing object. That is the one true
corruption. So gitmote always PUTs the objects first, and advances the ref after.

## 4. The write-loss window of s3lite is accepted

s3lite has a window in which it can lose writes. gitmote accepts it, because it is
benign for git. litestream replicates the SQLite WAL to S3 asynchronously. So if
the container dies within a window of under a second, s3lite can lose a committed
ref update.

Because of the order in §3, the loss always leaves objects without a ref, which is
garbage. It never leaves a ref without an object, which is corruption.

To the user, the loss is a lost acknowledgment. The client still holds its
commits. A re-push is cheap, because the objects are already in S3. The orphans
stay as harmless garbage (§3). Git is content-addressed and idempotent. So a
re-push of the same commits succeeds, and it restores the lost ref update. A
ledger cannot recover in this way. So gitmote accepts the window.

## 5. CI secrets are encrypted at rest, against a narrow threat

gitmote seals each per-repo CI secret with AES-256-GCM, under a master key that the
server holds. The master key is `GITMOTE_CI_SECRET_KEY_V<n>`, a container env var
beside the AWS keys. It lives only in the env, and gitmote never persists it (see §6).
HKDF-SHA256 derives a subkey for each repo. The AAD of each envelope binds
`(repoID, name, version)`. So an attacker cannot replay a stolen ciphertext under
another repo, name or version.

Only the sealed `{v, iv, ct}` lives in s3lite, never the plaintext or the key. This
gives one protection: a compromise of the S3 replica or a DB snapshot does not
leak secret values. This covers the litestream WAL and the object bucket.

The threat model is narrow. The encryption is explicitly not a defense against a
compromised running server. That server holds the master key by necessity. It must
decrypt the secrets to inject them into the runner env at trigger. There is no KDF
from a user password, unlike in an end-to-end (E2E) design. The server is the trust
boundary.

To rotate the key, add a new key version and bump the current version to it. Old
envelopes still decrypt under their own version. Values are write-only in the UI,
which shows only the names. gitmote never logs the values.

## 6. Two server secrets live in the replica, but not the CI master key

When no env var overrides them, gitmote generates two server secrets on first boot:

- the session cookie key, `GITMOTE_COOKIE_KEY`
- the CI worker secret, `WORKER_SECRET`

It persists both in `server_secrets`. So a container that scales to zero keeps
both across an idle period and a wake. Sessions stay valid, and in-flight CI stays
authenticated. A key per boot would log everyone out and orphan the runs.

This is acceptable for three reasons:

- The replica already holds users and tokens. It also holds the ACLs.
- Sessions are short-lived.
- The worker secret only authorizes CI report submission, never decryption.

By contrast, the CI master key stays env-only (§5). It decrypts the secrets of
every repo, so a replica leak must never expose it.

An explicit env var still wins for both server secrets. The leader generates each
one with get-or-create, because the DB of a follower is read-only. So restarts and
rolling deploys reuse the one restored value.

Two instances can boot together on a fresh root. Then a follower can find a secret
missing. It catches up from the replica until the secret appears, or it takes the
lease and creates the secret. It never writes as a follower.

## 7. Container image builds are an opt-in privilege escalation

Image builds are off by default. Workflow code is untrusted, because an attacker
can control it. The job container is the isolation boundary. Building an image
needs a container builder. The pragmatic path is the nested mode of `act`, which
mounts the host Docker socket into the job container. That grants the untrusted
code root privileges on the host, through a trivial container escape.

So by default, gitmote suppresses the mount with `act --container-daemon-socket -`,
in [`internal/runner/act.go`](../../internal/runner/act.go). It restores the mount
only when the operator sets `GITMOTE_CI_ALLOW_BUILDS`. The flag is deliberately
not per-repo. It is a trust decision at the host level. The operator makes it for a machine that
runs only repos they trust. An example is a personal or VPS forge that builds its
own code.

The flag has no effect on the Scaleway substrate. There, jobs run in self-hosted
mode, with no daemon and no socket to mount. Scaleway cannot build images anyway.
When the flag is on, the server logs a warning at startup. The warning says that
CI can reach the host Docker daemon, which is host root. It also says that the
flag is for trusted repos only.

### Why Docker-in-Docker does not replace the flag

Always using Docker-in-Docker (DinD) would not let gitmote drop the flag. DinD does
not remove the escape. It only moves it. A nested daemon needs a `--privileged`
container, which has:

- all capabilities
- the host devices
- unmasked `/proc` and `/sys`
- no seccomp

Such a container is itself equal to host root. Untrusted workflow code drives the
builder, so the code inherits that reach.

DinD gives a build its own daemon and cache. The mounted host socket (DooD) shares
the daemon and cache of the host. So the choice between them is about build state
isolation, not about safety against untrusted code. The axis that bounds untrusted
builds is rootless against rootful. On a rootful daemon, anything the build
reaches is real host root. A rootless engine caps the whole container tree at the
invoking unprivileged user, privileged nesting included.

So "always DinD where supported" would add machinery and add no safety:

- gitmote would have to provision, wire and manage a privileged daemon, and detect
  support for it.
- Scaleway cannot run privileged containers, so the same local and VPS set would
  remain.
- The operator-consent gate would stay. Builds enabled for any pushed repo grant
  host root by default.

A real fix at the root needs one of two additions:

- a rootless builder, such as rootless BuildKit, kaniko or buildah, where an
  escape lands as an unprivileged user
- a VM boundary for each job, such as Kata or Firecracker

gitmote has neither, so the consent gate is the only protection.
