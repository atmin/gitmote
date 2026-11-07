#!/usr/bin/env bash
# Local dev loop: MinIO runs in a container, and gitmote runs natively. The
# bootstrapped admin and repo persist under data/dev/ across restarts. Each run
# mints a fresh token. `make dev` invokes this script, and `make dev-reset`
# wipes the state.
set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
cd "$ROOT"

COMPOSE=(docker compose -p gitmote-dev --project-directory "$ROOT" -f "$ROOT/compose/dev.yml")

# All-local configuration. None of these values is secret. The db, the cache and
# the socket live under data/dev/, so refs survive restarts. That directory is
# gitignored, and it is the one GITMOTE_DATA directory. The bucket alone derives
# the metadata replica and the single-writer lease.
#
# The server listens on 6683, the canonical local port. 6683 spells MOTE on a
# phone keypad (README.md). Port 8080 of the container stays free for the e2e
# suites.
DEV_PORT=6683
export GITMOTE_LISTEN_ADDR=":$DEV_PORT"
export GITMOTE_S3_BUCKET="gitmote"
export GITMOTE_S3_ENDPOINT="http://localhost:9100"
export AWS_REGION="us-east-1"
export AWS_ACCESS_KEY_ID="minioadmin"
export AWS_SECRET_ACCESS_KEY="minioadmin"
export GITMOTE_DATA="$ROOT/data/dev"
export GITMOTE_COOKIE_KEY="dev-cookie-key-not-for-production"

# Enable local CI. On a push, gitmote records a run and spawns the runner
# (bin/gitmote-runner) as a local process. The cloud path uses the same runner
# code, but a Scaleway job drives it instead of the local trigger. The runner
# clones from GITMOTE_URL and reports back to it. WORKER_SECRET authenticates
# the report API. The runner executes .gitmote/workflows with `act`. To install
# act, run `brew install act`. act needs the Docker or podman daemon that MinIO
# already uses.
export GITMOTE_URL="http://localhost:$DEV_PORT"
export WORKER_SECRET="dev-worker-secret-not-for-production"

# Enable per-repo CI secrets locally, on the per-repo Secrets page. This is a
# fixed dev master key: base64 of 32 bytes, not for production. To rotate it,
# add _V2.
export GITMOTE_CI_SECRET_KEY_V1="ZGV2LW9ubHktY2ktc2VjcmV0LW1hc3Rlci1rZXktMzI="

mkdir -p "$ROOT/data/dev"

# Free the dev port from a stale dev server that a previous session left running.
if pid="$(lsof -nP -tiTCP:"$DEV_PORT" -sTCP:LISTEN 2>/dev/null)"; then
  echo "--- stopping process on :$DEV_PORT (pid $pid) ---"
  kill $pid 2>/dev/null || true
  for _ in $(seq 1 20); do
    lsof -nP -tiTCP:"$DEV_PORT" -sTCP:LISTEN >/dev/null 2>&1 || break
    sleep 0.1
  done
fi

echo "--- starting MinIO (S3 :9100, console :9101) ---"
"${COMPOSE[@]}" up -d minio
"${COMPOSE[@]}" run --rm mc

# Ensure that the admin and a dev repo to clone exist, then mint a fresh token to
# show. Both steps are idempotent, and both run before the server takes the
# lease:
#   - bootstrap creates the admin and the repo on the first run, and does
#     nothing after that.
#   - -reissue mints a new token for the existing admin on every run. So there
#     is no token file to keep in sync.
# The server also bootstraps itself on a truly empty bucket. This step makes the
# token and the dev repo available up front. 'make dev-reset' wipes the DB to
# start over.
echo "--- ensuring admin + dev repo (idempotent) ---"
bin/gitmote bootstrap -handle admin -repo gitmote -default-branch master >/dev/null
TOKEN="$(bin/gitmote bootstrap -reissue -handle admin | grep -oE 'gmt_[0-9a-f]+\.[0-9a-f]+' | head -1)"
[ -n "$TOKEN" ] || { echo "ERROR: no token from 'bootstrap -reissue'" >&2; exit 1; }

cat <<EOF

  gitmote dev is ready.

    UI:     http://localhost:$DEV_PORT/       (sign in at /login by pasting the token)
    token:  $TOKEN
    clone:  git clone http://admin:$TOKEN@localhost:$DEV_PORT/gitmote
    push:   git push http://admin:$TOKEN@localhost:$DEV_PORT/gitmote HEAD:refs/heads/master

  MinIO console: http://localhost:9101  (minioadmin / minioadmin)
  Ctrl-C stops the server; MinIO keeps running. 'make dev-reset' wipes all state.

  CI: push a repo with .gitmote/workflows and gitmote runs it locally via act
      ($(command -v act >/dev/null 2>&1 && echo 'act detected' || echo 'act NOT installed — run: brew install act')).

EOF

exec bin/gitmote
