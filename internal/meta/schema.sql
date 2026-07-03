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
  is_admin   INTEGER NOT NULL DEFAULT 0,       -- global admin: may manage users/repos/ACLs
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS tokens (              -- HTTP personal access tokens
  id         INTEGER PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id),
  selector   TEXT NOT NULL UNIQUE,                -- public lookup key (not secret)
  verifier   TEXT NOT NULL,                       -- SHA-256 of the token's secret half, never the raw token
  label      TEXT,
  created_at TEXT NOT NULL,
  last_used  TEXT,
  expires_at TEXT,                                -- NULL = never expires
  repo_scope INTEGER REFERENCES repos(id),        -- NULL = all the owner's repos; else the only allowed repo
  read_only  INTEGER NOT NULL DEFAULT 0           -- 1 = clone/fetch only, no push
);
-- NOTE: the three columns above are also added to existing DBs by the guarded
-- migration in meta.go (Metadata.migrate) — s3lite has no version table, so a
-- bare ALTER can't live here. That guarded ALTER is the pattern for future
-- additive columns.

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

-- CI: a queued/running/finished workflow run per ref advance, written only by
-- the leader (like refs). See docs/evolution/ci-runner.md and tasks 16/17.
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
  name        TEXT NOT NULL,                   -- workflow file / job name (filled in stage 2)
  status      TEXT NOT NULL,                   -- queued|running|passed|failed|error
  log_key     TEXT,                            -- ci/ object key, set on completion (stage 4)
  created_at  TEXT NOT NULL,
  updated_at  TEXT NOT NULL
);
