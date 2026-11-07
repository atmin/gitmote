#!/usr/bin/env bash
# End-to-end test that gitmote hosts itself. The script does these steps:
#   - Bring up MinIO and a gitmote container.
#   - Bootstrap an admin, a token and a repo.
#   - Push this working tree to it, clone it back, and assert that the clone is
#     byte-for-byte the pushed history.
#   - Force-recreate the container. The data volume, with its cache, stays.
#   - Clone again. This proves that the data survives a container restart.
#     make e2e-restore covers a restore without the volume.
set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
DC=(docker compose --project-directory "$ROOT" -f "$ROOT/compose/e2e.yml")
# Fall back to a fixed name when HEAD is detached, as it is in CI checkouts. The
# remote branch name is our own choice anyway.
BRANCH="$(git -C "$ROOT" symbolic-ref --quiet --short HEAD || echo master)"
PUSHED="$(git -C "$ROOT" rev-parse HEAD)"

# Fail fast instead of prompting; ignore any host credential helper.
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

fail() { echo "E2E FAIL: $*" >&2; exit 1; }

wait_healthz() {
  echo "--- waiting for gitmote /healthz ---"
  for _ in $(seq 1 60); do
    if curl -fsS http://localhost:8080/healthz >/dev/null 2>&1; then return 0; fi
    sleep 1
  done
  ( cd "$ROOT" && "${DC[@]}" logs gitmote ) || true
  fail "gitmote did not become healthy"
}

# assert_clone <label>: clone the repo fresh, and check HEAD, fsck and the tree.
assert_clone() {
  local label="$1" clone
  clone="$(mktemp -d)"
  CLONES+=("$clone")
  echo "--- clone ($label) ---"
  "${GITC[@]}" clone "$REPO_URL" "$clone" || fail "$label: clone failed"

  local cloned
  cloned="$(git -C "$clone" rev-parse HEAD)"
  [ "$cloned" = "$PUSHED" ] || fail "$label: HEAD $cloned != pushed $PUSHED"

  git -C "$clone" fsck --full --strict || fail "$label: fsck not clean"

  diff <(git -C "$ROOT" ls-tree -r "$PUSHED") <(git -C "$clone" ls-tree -r HEAD) \
    || fail "$label: file tree differs from pushed commit"
  echo "--- clone ($label) OK: HEAD=$cloned, fsck clean, tree identical ---"
}

cd "$ROOT"

echo "--- building gitmote image ---"
"${DC[@]}" build gitmote

echo "--- starting minio + creating bucket ---"
"${DC[@]}" up -d minio
"${DC[@]}" run --rm mc

echo "--- bootstrap (one-shot, writes admin/token/repo to the metadata volume) ---"
BOOT="$("${DC[@]}" run --rm -T gitmote bootstrap -handle admin -repo gitmote -default-branch "$BRANCH")"
TOKEN="$(printf '%s\n' "$BOOT" | grep -oE 'gmt_[0-9a-f]+\.[0-9a-f]+' | head -1)"
[ -n "$TOKEN" ] || { printf '%s\n' "$BOOT"; fail "no token in bootstrap output"; }

REPO_URL="http://admin:${TOKEN}@localhost:8080/gitmote"

echo "--- starting gitmote server ---"
"${DC[@]}" up -d gitmote
wait_healthz

echo "--- push this working tree ($BRANCH @ $PUSHED) ---"
"${GITC[@]}" -C "$ROOT" push "$REPO_URL" "HEAD:refs/heads/${BRANCH}" || fail "push failed"

assert_clone "initial"

echo "--- force-recreate container (wipes ephemeral cache; refs+objects persist) ---"
"${DC[@]}" up -d --force-recreate --no-deps gitmote
wait_healthz
assert_clone "after-restart"

echo "E2E PASS: gitmote hosted itself — push, clone, and restart-durability all green."
