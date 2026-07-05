#!/usr/bin/env bash
# Local dev loop: MinIO in a container + a natively-run gitmote, with a
# bootstrapped admin/repo persisted under data/ across restarts and a fresh token
# minted on each run. Invoked by `make dev`; `make dev-reset` wipes the state.
set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
cd "$ROOT"

COMPOSE=(docker compose -p gitmote-dev -f docker-compose.dev.yml)

# All-local configuration — none of these are secret. The db, cache, and socket
# live under data/ (gitignored, one GITMOTE_DATA dir) so refs survive restarts.
# The bucket alone now derives the metadata replica + single-writer lease.
export GITMOTE_S3_BUCKET="gitmote"
export GITMOTE_S3_ENDPOINT="http://localhost:9100"
export AWS_REGION="us-east-1"
export AWS_ACCESS_KEY_ID="minioadmin"
export AWS_SECRET_ACCESS_KEY="minioadmin"
export GITMOTE_DATA="$ROOT/data"
export GITMOTE_HOOK="$ROOT/bin/gitmote-hook"
export GITMOTE_RUNNER="$ROOT/bin/gitmote-runner"
export GITMOTE_COOKIE_KEY="dev-cookie-key-not-for-production"

# Enable local CI: on a push, gitmote records a run and spawns the runner
# (bin/gitmote-runner) as a local process — the same runner code the cloud path
# uses, driven by the local trigger instead of a Scaleway job. GITMOTE_URL is
# where the runner clones + reports back; WORKER_SECRET authenticates the report
# API. The runner executes .github/workflows with `act` (install: brew install
# act), which needs the Docker/podman daemon MinIO already relies on.
export GITMOTE_URL="http://localhost:8080"
export WORKER_SECRET="dev-worker-secret-not-for-production"

# Enable per-repo CI secrets locally (per-repo Secrets page). A fixed dev master
# key — base64 of 32 bytes — obviously not for production. Rotate by adding _V2.
export GITMOTE_CI_SECRET_KEY_V1="ZGV2LW9ubHktY2ktc2VjcmV0LW1hc3Rlci1rZXktMzI="

mkdir -p "$ROOT/data"

# Free :8080 from a stale dev server left running from a previous session.
if pid="$(lsof -nP -tiTCP:8080 -sTCP:LISTEN 2>/dev/null)"; then
  echo "--- stopping process on :8080 (pid $pid) ---"
  kill $pid 2>/dev/null || true
  for _ in $(seq 1 20); do
    lsof -nP -tiTCP:8080 -sTCP:LISTEN >/dev/null 2>&1 || break
    sleep 0.1
  done
fi

echo "--- starting MinIO (S3 :9100, console :9101) ---"
"${COMPOSE[@]}" up -d minio
"${COMPOSE[@]}" run --rm mc

# Ensure the admin (+ a dev repo to clone) exist, then mint a fresh token to
# show. Both steps are idempotent and run before the server takes the lease:
# bootstrap creates on first run and no-ops after; -reissue mints a new token for
# the existing admin every run, so there is no token file to keep in sync (the
# server also auto-bootstraps on a truly empty bucket — this just makes the token
# and the dev repo available up front). 'make dev-reset' wipes the DB to start over.
echo "--- ensuring admin + dev repo (idempotent) ---"
bin/gitmote bootstrap -handle atmin -repo gitmote -default-branch master >/dev/null
TOKEN="$(bin/gitmote bootstrap -reissue -handle atmin | grep -oE 'gmt_[0-9a-f]+\.[0-9a-f]+' | head -1)"
[ -n "$TOKEN" ] || { echo "ERROR: no token from 'bootstrap -reissue'" >&2; exit 1; }

cat <<EOF

  gitmote dev is ready.

    UI:     http://localhost:8080/       (sign in at /login by pasting the token)
    token:  $TOKEN
    clone:  git clone http://atmin:$TOKEN@localhost:8080/gitmote
    push:   git push http://atmin:$TOKEN@localhost:8080/gitmote HEAD:refs/heads/master

  MinIO console: http://localhost:9101  (minioadmin / minioadmin)
  Ctrl-C stops the server; MinIO keeps running. 'make dev-reset' wipes all state.

  CI: push a repo with .github/workflows and gitmote runs it locally via act
      ($(command -v act >/dev/null 2>&1 && echo 'act detected' || echo 'act NOT installed — run: brew install act')).

EOF

exec bin/gitmote
