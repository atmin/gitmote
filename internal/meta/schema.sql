-- The gitmote metadata schema: the mutable forge state, as described in
-- docs/architecture/storage.md. It runs on every Open, so every statement is
-- idempotent (CREATE ... IF NOT EXISTS). s3lite has no version table.

CREATE TABLE IF NOT EXISTS repos (
  id             INTEGER PRIMARY KEY,
  name           TEXT NOT NULL UNIQUE,          -- a single path segment, for example "gitmote"
  default_branch TEXT NOT NULL DEFAULT 'main',
  visibility     TEXT NOT NULL DEFAULT 'private'   -- 'private' or 'public'. Anyone may read a public repo anonymously.
                 CHECK (visibility IN ('private','public')),
  created_at     TEXT NOT NULL
);

-- The mutable pointers. They are the whole reason that this DB exists in the design.
CREATE TABLE IF NOT EXISTS refs (
  repo_id    INTEGER NOT NULL REFERENCES repos(id),
  name       TEXT NOT NULL,                      -- "refs/heads/main"
  sha        TEXT NOT NULL,                       -- object id
  updated_at TEXT NOT NULL,
  PRIMARY KEY (repo_id, name)
);

CREATE TABLE IF NOT EXISTS users (
  id         INTEGER PRIMARY KEY,
  handle     TEXT NOT NULL UNIQUE,
  is_admin   INTEGER NOT NULL DEFAULT 0,       -- global admin: may manage users, repos and ACLs
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS tokens (              -- HTTP personal access tokens
  id         INTEGER PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id),
  selector   TEXT NOT NULL UNIQUE,                -- public lookup key (not secret)
  verifier   TEXT NOT NULL,                       -- SHA-256 of the secret half of the token, never the raw token
  label      TEXT,
  created_at TEXT NOT NULL,
  last_used  TEXT,
  expires_at TEXT,                                -- NULL = never expires
  repo_scope INTEGER REFERENCES repos(id),        -- NULL = all repos of the owner; otherwise the only allowed repo
  read_only  INTEGER NOT NULL DEFAULT 0           -- 1 = clone and fetch only, no push
);
-- NOTE: the guarded migration in meta.go (Metadata.migrate) also adds the three
-- columns above to existing DBs. s3lite has no version table, so a bare ALTER
-- can't live here. To add a column to an existing table, use that guarded ALTER.

CREATE TABLE IF NOT EXISTS ssh_keys (            -- no code reads this table
  id         INTEGER PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id),
  pubkey     TEXT NOT NULL,                        -- OpenSSH authorized_keys line
  label      TEXT,
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS acls (
  repo_id    INTEGER NOT NULL REFERENCES repos(id),
  user_id    INTEGER NOT NULL REFERENCES users(id),
  perm       TEXT NOT NULL CHECK (perm IN ('read','write','admin')),
  PRIMARY KEY (repo_id, user_id)
);

-- CI: one workflow run for each ref advance, queued, running or finished. Only the
-- leader writes it, as with refs. See docs/architecture/ci.md.
CREATE TABLE IF NOT EXISTS ci_runs (
  id         INTEGER PRIMARY KEY,
  repo_id    INTEGER NOT NULL REFERENCES repos(id),
  ref        TEXT NOT NULL,                    -- "refs/heads/main"
  sha        TEXT NOT NULL,                    -- the new tip
  status     TEXT NOT NULL,                    -- queued|running|passed|failed|error|superseded
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS ci_jobs (
  id          INTEGER PRIMARY KEY,
  run_id      INTEGER NOT NULL REFERENCES ci_runs(id),
  name        TEXT NOT NULL,                   -- the job name: the base name of the workflow file
  status      TEXT NOT NULL,                   -- queued|running|passed|failed|error
  log_key     TEXT,                            -- the ci/ object key, set on completion
  created_at  TEXT NOT NULL,
  updated_at  TEXT NOT NULL
);

-- CI secrets: named values per repo, encrypted at rest with a master key that the
-- server holds. The master key is never stored here. Only the sealed envelope
-- lives in the DB: {version, iv, ciphertext}. So a leak of a replica or a
-- snapshot does not expose the values. The UI can only write values, and it lists
-- only names. See internal/secrets.
CREATE TABLE IF NOT EXISTS ci_secrets (
  repo_id    INTEGER NOT NULL REFERENCES repos(id),
  name       TEXT NOT NULL,                    -- env var name injected at trigger
  v          INTEGER NOT NULL,                 -- master key version that sealed ct
  iv         BLOB NOT NULL,                    -- 12-byte GCM nonce
  ct         BLOB NOT NULL,                    -- AES-256-GCM ciphertext + tag
  created_at TEXT NOT NULL,
  PRIMARY KEY (repo_id, name)
);

-- This table holds the server secrets: the session cookie key and the CI worker
-- secret. gitmote creates them on the first boot, unless the env overrides them. A key that persists,
-- unlike a key per boot, keeps sessions valid and in-flight CI authenticated.
-- This holds across a scale-to-zero idle and wake. The table deliberately holds
-- only these two named keys. It is not a general KV store. The CI *master* key is
-- NOT here. It stays env-only (docs/architecture/safety.md §5). See
-- Metadata.GetOrCreateSecret and cmd/gitmote.
CREATE TABLE IF NOT EXISTS server_secrets (
  name  TEXT PRIMARY KEY,                      -- "cookie_key", "worker_secret"
  value BLOB NOT NULL
);
