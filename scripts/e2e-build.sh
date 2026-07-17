#!/usr/bin/env bash
# End-to-end proof that GITMOTE_CI_ALLOW_BUILDS gates real container image builds.
#
# gitmote's runner (internal/runner/act.go, actArgs) runs `act` in nested mode and,
# unless GITMOTE_CI_ALLOW_BUILDS is truthy, passes `--container-daemon-socket -` to
# suppress act's default host-Docker-socket mount. The unit tests prove actArgs
# emits exactly that flag (off) or omits it (on); this script proves those two
# arg-states actually flip a real `docker build` from failing (no daemon access)
# to succeeding — the other half of the chain.
#
# It runs everything inside one privileged docker-in-docker container with its own
# rootful daemon — "a Linux VPS with docker" in a box — so it works even where the
# host engine is rootless podman (whose socket-mount permissions otherwise block
# the job container; that's a rootless-userns quirk, not a gitmote behaviour).
#
# Requires a reachable Docker/podman daemon. Skips (exit 0) when none is present —
# no daemon means no CI and no builds anyway, which is the point. Not part of
# `make all`; run explicitly with `make e2e-build`.
set -euo pipefail

# Pin act to the version baked into the runner image (Dockerfile.runner).
ACT_VERSION="v0.2.89"
CN="gitmote-e2e-build"
# A small job image that ships the docker CLI, so the build step can reach the
# daemon without pulling act's ~1GB default runner image. The socket-mount gate
# under test is identical regardless of which job image is used.
JOB_IMAGE="docker:cli"

DOCKER="${DOCKER:-docker}"

if ! "$DOCKER" info >/dev/null 2>&1; then
  echo "SKIP: no reachable Docker/podman daemon (nothing to build against)."
  exit 0
fi

cleanup() {
  local code=$?
  "$DOCKER" rm -f "$CN" >/dev/null 2>&1 || true
  exit $code
}
trap cleanup EXIT

fail() { echo "E2E BUILD FAIL: $*" >&2; exit 1; }

echo "--- starting privileged DinD box (own rootful daemon) ---"
"$DOCKER" rm -f "$CN" >/dev/null 2>&1 || true
"$DOCKER" run -d --privileged --name "$CN" docker:dind >/dev/null
for _ in $(seq 1 30); do
  "$DOCKER" exec "$CN" docker info >/dev/null 2>&1 && break
  sleep 1
done
"$DOCKER" exec "$CN" docker info >/dev/null 2>&1 || fail "inner daemon never came up"
echo "inner daemon: $("$DOCKER" exec "$CN" docker version --format '{{.Server.Version}}'), uid $("$DOCKER" exec "$CN" id -u)"

echo "--- installing act $ACT_VERSION + tools inside ---"
"$DOCKER" exec "$CN" sh -c 'apk add --no-cache bash git curl tar >/dev/null'
arch="$("$DOCKER" exec "$CN" uname -m)"
case "$arch" in
  aarch64) a=arm64 ;;
  x86_64)  a=x86_64 ;;
  *)       a="$arch" ;;
esac
"$DOCKER" exec "$CN" sh -c \
  "curl -fsSL https://github.com/nektos/act/releases/download/${ACT_VERSION}/act_Linux_${a}.tar.gz | tar -xz -C /usr/local/bin act" \
  || fail "could not install act"
"$DOCKER" exec "$CN" docker pull -q "$JOB_IMAGE" >/dev/null || fail "could not pull $JOB_IMAGE"

echo "--- writing a workflow that builds an image ---"
"$DOCKER" exec "$CN" sh -c 'mkdir -p /repo/.gitmote/workflows; cat > /repo/.gitmote/workflows/build.yml <<YML
name: build
on: push
defaults:
  run:
    shell: sh
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: |
          printf "FROM alpine\nRUN echo IMAGE_BUILT_OK\n" > Dockerfile
          docker build -t probe .
YML'

# actArgs always passes --workflows; the only difference between off and on is the
# socket-suppression flag below.
act_off() { "$DOCKER" exec -w /repo "$CN" act --workflows .gitmote/workflows -P "ubuntu-latest=$JOB_IMAGE" --container-daemon-socket - push; }
act_on()  { "$DOCKER" exec -w /repo "$CN" act --workflows .gitmote/workflows -P "ubuntu-latest=$JOB_IMAGE" push; }

echo "--- builds OFF (default: --container-daemon-socket -) — expect FAIL ---"
if act_off >/dev/null 2>&1; then
  fail "build succeeded with the socket suppressed (gate leaks)"
fi
echo "OK: build failed with no daemon access (as designed)"

echo "--- builds ON (GITMOTE_CI_ALLOW_BUILDS=1: socket mounted) — expect PASS ---"
act_on >/dev/null 2>&1 || fail "build failed even with daemon access"
"$DOCKER" exec "$CN" docker image inspect probe >/dev/null 2>&1 || fail "no image produced"
echo "OK: image built and present in the daemon ($("$DOCKER" exec "$CN" docker image inspect probe --format '{{.RepoTags}}'))"

echo "E2E BUILD PASS: the socket gate flips a real image build off/on as designed."
