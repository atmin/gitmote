-- gitmote metadata schema — the mutable forge state, per
-- docs/architecture/storage.md. Runs on every Open, so every statement is
-- idempotent (CREATE ... IF NOT EXISTS); s3lite has no version table.

CREATE TABLE IF NOT EXISTS repos (
  id             INTEGER PRIMARY KEY,
  name           TEXT NOT NULL UNIQUE,          -- "atmin/dotfiles"
  default_branch TEXT NOT NULL DEFAULT 'main',
  created_at     TEXT NOT NULL
);

-- the mutable pointers — the whole reason this DB exists in the design
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
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS tokens (              -- HTTP personal access tokens
  id         INTEGER PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id),
  hash       TEXT NOT NULL,                       -- hash of the PAT, never the raw token
  label      TEXT,
  created_at TEXT NOT NULL,
  last_used  TEXT
);

CREATE TABLE IF NOT EXISTS ssh_keys (            -- deferred transport, schema ready
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
