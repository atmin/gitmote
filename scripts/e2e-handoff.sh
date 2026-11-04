#!/usr/bin/env bash
# Prove that two live instances on one storage root take turns as the writer
# (docs/ops.md → the idle lease handoff). A receives a push, idles, and yields the
# lease; B then serves that push. B moves one branch and deletes another, and A —
# whose warm cache still holds the loose refs of its own push — must advertise
# exactly B's refs. Whenever both are idle, no instance holds the lease.
set -euo pipefail

DC="docker compose --profile handoff"
ROOT="$(git rev-parse --show-toplevel)"
BRANCH="$(git -C "$ROOT" symbolic-ref --quiet --short HEAD || echo master)"
PUSHED="$(git -C "$ROOT" rev-parse HEAD)"

export GIT_TERMINAL_PROMPT=0
GITC=(git -c credential.helper= -c http.postBuffer=524288000 -c user.name=e2e -c user.email=e2e@example.com)

CLONES=()
cleanup() {
  local code=$?
  echo "--- tearing down ---"
  ( cd "$ROOT" && $DC down -v --remove-orphans ) || true
  for d in "${CLONES[@]:-}"; do [ -n "$d" ] && rm -rf "$d"; done
  exit $code
}
trap cleanup EXIT

fail() {
  echo "HANDOFF FAIL: $*" >&2
  ( cd "$ROOT" && $DC logs gitmote gitmote-b ) || true
  exit 1
}

wait_healthz() {
  local port="$1" svc="$2"
  for _ in $(seq 1 60); do
    curl -fsS "http://localhost:${port}/healthz" >/dev/null 2>&1 && return 0
    sleep 1
  done
  fail "$svc did not become healthy"
}

# wait_lease_free waits until no instance holds the writer lease. An idle writer
# yields after 5 s, and the yield deletes meta/lock.json, so the file must vanish
# within seconds. A failed listing is retried, never read as "free".
wait_lease_free() {
  local out
  for _ in $(seq 1 30); do
    if out="$($DC run --rm -T mc "mc ls local/gitmote/meta/" 2>/dev/null)"; then
      grep -q 'lock.json' <<<"$out" || return 0
    fi
    sleep 1
  done
  fail "an instance still holds the writer lease after 30 s idle"
}

# clone_into clones url into a fresh temp dir, recorded for cleanup, and checks it.
clone_into() {
  local url="$1" dir="$2"
  "${GITC[@]}" clone -q "$url" "$dir" || fail "clone from ${url##*@} failed"
  git -C "$dir" fsck --full --strict || fail "fsck of the clone from ${url##*@} is not clean"
}

cd "$ROOT"

echo "--- build + minio + bucket ---"
$DC build gitmote
$DC up -d minio
$DC run --rm mc

echo "--- bootstrap ---"
BOOT="$($DC run --rm -T gitmote bootstrap -handle admin -repo gitmote -default-branch "$BRANCH")"
TOKEN="$(printf '%s\n' "$BOOT" | grep -oE 'gmt_[0-9a-f]+\.[0-9a-f]+' | head -1)"
[ -n "$TOKEN" ] || { printf '%s\n' "$BOOT"; fail "no token in bootstrap output"; }
A="http://admin:${TOKEN}@localhost:8080/gitmote"
B="http://admin:${TOKEN}@localhost:8081/gitmote"

echo "--- start A (:8080) and B (:8081) on one storage root ---"
$DC up -d gitmote gitmote-b
wait_healthz 8080 gitmote
wait_healthz 8081 gitmote-b
wait_lease_free

echo "--- push to A: $BRANCH and doomed @ $PUSHED ---"
"${GITC[@]}" -C "$ROOT" push "$A" "${PUSHED}:refs/heads/${BRANCH}" "${PUSHED}:refs/heads/doomed" \
  || fail "push to A failed"

echo "--- A yields when idle; B serves A's push ---"
wait_lease_free
CLONE="$(mktemp -d)"; CLONES+=("$CLONE")
clone_into "$B" "$CLONE"
[ "$(git -C "$CLONE" rev-parse HEAD)" = "$PUSHED" ] || fail "B serves $(git -C "$CLONE" rev-parse HEAD), want A's push $PUSHED"

echo "--- B moves $BRANCH and deletes doomed ---"
"${GITC[@]}" -C "$CLONE" commit -q --allow-empty -m "e2e: handoff"
MOVED="$(git -C "$CLONE" rev-parse HEAD)"
"${GITC[@]}" -C "$CLONE" push "$B" "${MOVED}:refs/heads/${BRANCH}" ":refs/heads/doomed" || fail "push to B failed"

echo "--- B yields; A, its cache warm with the old refs, advertises exactly B's ---"
wait_lease_free
GOT="$("${GITC[@]}" ls-remote "$A" 'refs/heads/*')" || fail "ls-remote against A failed"
WANT="$(printf '%s\trefs/heads/%s' "$MOVED" "$BRANCH")"
[ "$GOT" = "$WANT" ] || fail "A advertises:
$GOT
want exactly:
$WANT"
CLONE_A="$(mktemp -d)"; CLONES+=("$CLONE_A")
clone_into "$A" "$CLONE_A"
[ "$(git -C "$CLONE_A" rev-parse HEAD)" = "$MOVED" ] || fail "A serves $(git -C "$CLONE_A" rev-parse HEAD), want B's push $MOVED"

echo "--- both instances logged the yield ---"
for svc in gitmote gitmote-b; do
  # Capture first: under pipefail, grep -q closing the pipe early fails the logs call.
  LOGS="$($DC logs "$svc" 2>/dev/null)"
  grep -q 'yielded the writer lease' <<<"$LOGS" || fail "$svc never logged a yield"
done

echo "HANDOFF PASS: two instances took turns as the writer — pushes on either side served by the other, no stale ref, no lease pinned while idle."
