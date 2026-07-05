#!/usr/bin/env bash
# Prove the litestream cold-start path before trusting it in production: push a
# repo with metadata replication on, gracefully stop the server (flushing the WAL
# to S3), then DELETE the metadata volume — simulating a fresh container on
# ephemeral disk — and bring it back. The clone must still succeed, which is only
# possible if refs were restored from the object store, not local disk.
set -euo pipefail

DC="docker compose"
ROOT="$(git rev-parse --show-toplevel)"
BRANCH="$(git -C "$ROOT" symbolic-ref --quiet --short HEAD || echo master)"
PUSHED="$(git -C "$ROOT" rev-parse HEAD)"

# Turn on replication for this run (docker-compose.yml reads it; empty otherwise).
export GITMOTE_DB_REPLICA="s3://gitmote/meta"
DB_VOLUME="gitmote-e2e_gitmote-data"

export GIT_TERMINAL_PROMPT=0
GITC=(git -c credential.helper= -c http.postBuffer=524288000)

CLONES=()
cleanup() {
  local code=$?
  echo "--- tearing down ---"
  ( cd "$ROOT" && $DC down -v --remove-orphans ) || true
  for d in "${CLONES[@]:-}"; do [ -n "$d" ] && rm -rf "$d"; done
  exit $code
}
trap cleanup EXIT

fail() { echo "RESTORE FAIL: $*" >&2; exit 1; }

wait_healthz() {
  for _ in $(seq 1 60); do
    curl -fsS http://localhost:8080/healthz >/dev/null 2>&1 && return 0
    sleep 1
  done
  ( cd "$ROOT" && $DC logs gitmote ) || true
  fail "gitmote did not become healthy"
}

cd "$ROOT"

echo "--- build + minio + bucket ---"
$DC build gitmote
$DC up -d minio
$DC run --rm mc

echo "--- bootstrap (replicates initial metadata to S3) ---"
BOOT="$($DC run --rm -T gitmote bootstrap -handle atmin -repo gitmote -default-branch "$BRANCH")"
TOKEN="$(printf '%s\n' "$BOOT" | grep -oE 'gmt_[0-9a-f]+\.[0-9a-f]+' | head -1)"
[ -n "$TOKEN" ] || { printf '%s\n' "$BOOT"; fail "no token in bootstrap output"; }
REPO_URL="http://atmin:${TOKEN}@localhost:8080/gitmote"

echo "--- start server + push ($BRANCH @ $PUSHED) ---"
$DC up -d gitmote
wait_healthz
"${GITC[@]}" -C "$ROOT" push "$REPO_URL" "HEAD:refs/heads/${BRANCH}" || fail "push failed"

echo "--- graceful stop (flushes WAL to S3), then DESTROY the metadata volume ---"
$DC stop gitmote                       # SIGTERM → graceful shutdown → replication flush
$DC rm -f gitmote                      # remove the container so the volume is free
docker volume rm "$DB_VOLUME"          # simulate a fresh container: local DB is gone
docker volume ls --format '{{.Name}}' | grep -qx "$DB_VOLUME" \
  && fail "metadata volume still present — wipe did not take"

echo "--- cold start: DB restored from S3, cache rebuilt from S3 ---"
$DC up -d gitmote
wait_healthz

clone="$(mktemp -d)"; CLONES+=("$clone")
"${GITC[@]}" clone "$REPO_URL" "$clone" || fail "clone after restore failed"
cloned="$(git -C "$clone" rev-parse HEAD)"
[ "$cloned" = "$PUSHED" ] || fail "restored HEAD $cloned != pushed $PUSHED"
git -C "$clone" fsck --full --strict || fail "restored repo fsck not clean"
diff <(git -C "$ROOT" ls-tree -r "$PUSHED") <(git -C "$clone" ls-tree -r HEAD) \
  || fail "restored file tree differs from pushed commit"

echo "RESTORE PASS: metadata volume wiped, refs restored from S3 — HEAD=$cloned, fsck clean, tree identical."
