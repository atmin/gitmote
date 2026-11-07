#!/usr/bin/env bash
# Test the litestream cold-start path before production trusts it:
#   - Push a repo with metadata replication on.
#   - Stop the server gracefully, which flushes the WAL to S3.
#   - DELETE the metadata volume. This simulates a fresh container on ephemeral
#     disk.
#   - Bring the server back.
# The clone must still succeed. That is only possible if the refs were restored
# from the object store, not from local disk.
set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
DC=(docker compose --project-directory "$ROOT" -f "$ROOT/compose/e2e.yml")
BRANCH="$(git -C "$ROOT" symbolic-ref --quiet --short HEAD || echo master)"
PUSHED="$(git -C "$ROOT" rev-parse HEAD)"

# Replication is on whenever a bucket is set, because the storage root derives
# the replica. So there is nothing to turn on here. There is only the volume to
# wipe in the middle of the run.
DB_VOLUME="gitmote-e2e_gitmote-data"

export GIT_TERMINAL_PROMPT=0
GITC=(git -c credential.helper= -c http.postBuffer=524288000)

CLONES=()
cleanup() {
  local code=$?
  echo "--- tearing down ---"
  ( cd "$ROOT" && "${DC[@]}" down -v --remove-orphans ) || true
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
  ( cd "$ROOT" && "${DC[@]}" logs gitmote ) || true
  fail "gitmote did not become healthy"
}

cd "$ROOT"

echo "--- build + minio + bucket ---"
"${DC[@]}" build gitmote
"${DC[@]}" up -d minio
"${DC[@]}" run --rm mc

echo "--- bootstrap (replicates initial metadata to S3) ---"
BOOT="$("${DC[@]}" run --rm -T gitmote bootstrap -handle admin -repo gitmote -default-branch "$BRANCH")"
TOKEN="$(printf '%s\n' "$BOOT" | grep -oE 'gmt_[0-9a-f]+\.[0-9a-f]+' | head -1)"
[ -n "$TOKEN" ] || { printf '%s\n' "$BOOT"; fail "no token in bootstrap output"; }
REPO_URL="http://admin:${TOKEN}@localhost:8080/gitmote"

echo "--- start server + push ($BRANCH @ $PUSHED) ---"
"${DC[@]}" up -d gitmote
wait_healthz
"${GITC[@]}" -C "$ROOT" push "$REPO_URL" "HEAD:refs/heads/${BRANCH}" || fail "push failed"

echo "--- graceful stop (flushes WAL to S3), then DESTROY the metadata volume ---"
"${DC[@]}" stop gitmote                       # SIGTERM starts a graceful shutdown, which flushes replication
"${DC[@]}" rm -f gitmote                      # remove the container so the volume is free
docker volume rm "$DB_VOLUME"          # simulate a fresh container: the local DB is gone
docker volume ls --format '{{.Name}}' | grep -qx "$DB_VOLUME" \
  && fail "metadata volume still present — wipe did not take"

echo "--- cold start: DB restored from S3, cache rebuilt from S3 ---"
"${DC[@]}" up -d gitmote
wait_healthz

clone="$(mktemp -d)"; CLONES+=("$clone")
"${GITC[@]}" clone "$REPO_URL" "$clone" || fail "clone after restore failed"
cloned="$(git -C "$clone" rev-parse HEAD)"
[ "$cloned" = "$PUSHED" ] || fail "restored HEAD $cloned != pushed $PUSHED"
git -C "$clone" fsck --full --strict || fail "restored repo fsck not clean"
diff <(git -C "$ROOT" ls-tree -r "$PUSHED") <(git -C "$clone" ls-tree -r HEAD) \
  || fail "restored file tree differs from pushed commit"

echo "RESTORE PASS: metadata volume wiped, refs restored from S3 — HEAD=$cloned, fsck clean, tree identical."
