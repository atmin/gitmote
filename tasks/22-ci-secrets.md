# CI stage 5 — encrypted per-repo secrets store

Part of the CI epic ([16-ci.md](16-ci.md), decision §Stage 0 #3 — **locked**).
Depends on the dispatcher building the trigger env (stage 3
[19](19-ci-scaleway-trigger.md)). Adds per-repo CI secrets, encrypted at rest,
injected into the runner env at trigger. This is the one genuinely new security
surface — treat it with care.

## Spec

Per-repo named secrets that an admin can set/delete (write-only — values are
never shown again). At trigger time the dispatcher decrypts the repo's secrets and
adds them to the Scaleway job env. Encrypted at rest with a **server-held** master
key so a compromise of the S3 replica / DB snapshot does not leak secrets — but
**not** a defense against a compromised running server. State that honestly in the
safety doc; do not oversell it.

## Current

- No secret store. The dispatcher (stage 3) already assembles a `map[string]string`
  env for `Trigger` — the injection point.
- Prior-art crypto in `~/dev/atmin.net` (adapt, don't copy — its trust model is
  inverted, being end-to-end with a password-derived key):
  `web/src/lib/crypto.ts` (AES-256-GCM, 12-byte random IV; HKDF-per-purpose),
  `key-chain.ts` + ADR-0012 (versioned rotation). See epic §Stage 0 #3.
- Container secret env is the home for the master key, alongside
  `GITMOTE_COOKIE_KEY` / AWS keys ([ops.md](../docs/ops.md)).

## Change

**1. `internal/secrets` package** — pure-crypto, unit-testable:
- **Master keys from env, versioned:** `GITMOTE_CI_SECRET_KEY_V1` (base64, 32
  bytes), `…_V2`, … The process holds a `map[version][32]byte`; the highest
  present is *current*. **Because the keys are all server-held, we keep a
  version→key map rather than atmin.net's on-disk wrapped key-chain** — same lazy,
  O(1)-rotation property (each envelope decrypts with its own version's key), far
  simpler. Rotation = add a new key version + bump current; no re-encryption.
- **Per-repo subkey:** `HKDF-SHA256(masterKey[v], info="ci-secret:"+repoID)` →
  the AES key. (Isolates repos under one master, mirrors atmin.net's
  `hkdfDerive(secret, info)`.)
- **Encrypt/Decrypt:** AES-256-GCM (`crypto/aes`+`crypto/cipher`), 12-byte IV from
  `crypto/rand` per encryption. **AAD = repoID ‖ name ‖ version** (the improvement
  over the prior art — binds the envelope so a ciphertext can't be replayed under
  a different repo/name/version). Envelope = `{v, iv, ct}`.
- **Fail closed:** decrypt requested but the needed key version is absent → a
  clear error; never a silent empty or a panic.

**2. `meta` + schema** — `ci_secrets(repo_id, name, v, iv, ct, created_at,
PRIMARY KEY(repo_id,name))` (idempotent `CREATE TABLE IF NOT EXISTS`). Helpers:
`SetSecret` (upsert the envelope), `ListSecretNames(repoID)` (names only — never
values), `GetSecrets(repoID)` (envelopes, for decryption at trigger), `DeleteSecret`.

**3. Wire into the dispatcher** — at trigger, `GetSecrets(repoID)` → decrypt each
→ add to the job env map. Never log secret names+values; scrub from any error
strings.

**4. Admin UI** ([internal/webui](../internal/webui)) — a per-repo secrets panel:
list **names** + set (name+value form) + delete, behind `requireAdmin`. The set
form is write-only; the value is never rendered back.

## Verify

- **`secrets` unit tests:** encrypt→decrypt round-trip; a decrypt with the wrong
  repoID/name/version (i.e. wrong AAD) **fails** on the auth tag; two encryptions
  of the same plaintext differ (fresh IV); a value with the needed key version
  absent → fail-closed error.
- **rotation:** a secret written under `V1` still decrypts after `V2` is added and
  becomes current; new writes use `V2`.
- **leak checks (safety invariant, explicit test):** a set secret's **value**
  never appears in `ListSecretNames`, in any rendered UI page, or in logs; master
  key never logged.
- **webui route tests:** set/list/delete behind `requireAdmin`; unauth → denied;
  the value isn't echoed in the response.
- `gofmt`/`golangci-lint`/`go test ./...` clean; e2e green.
