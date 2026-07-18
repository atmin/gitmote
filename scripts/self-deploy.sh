#!/usr/bin/env bash
# Break-glass self-deploy: gitmote builds and deploys itself through its own local
# runner, with zero manual secret entry. It runs a throwaway gitmote instance
# natively (MinIO in a container on non-dev ports), seeds the deploy secrets from a
# gitignored .env as GITMOTE_REPO_SECRET_GITMOTE__* (no Secrets UI, no master key),
# and pushes HEAD to the `self-deploy` branch — which triggers
# .gitmote/workflows/deploy.yml to build the amd64 image, push it to GHCR, and
# `scw container update` prod. Invoked by `make self-deploy`. See docs/ops.md.
#
# The everyday deployer stays GitHub Actions on `master`; this is the fallback for
# when that's unavailable or an out-of-band deploy is wanted.
set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
cd "$ROOT"

PORT=8081
S3_PORT=9200
DATA="$ROOT/data-selfdeploy"
COMPOSE=(docker compose -p gitmote-selfdeploy -f docker-compose.selfdeploy.yml)

# --- 1. Load the gitignored .env holding the deploy secrets -------------------
if [ ! -f "$ROOT/.env" ]; then
  echo "ERROR: no .env — copy .env.example to .env and fill in the deploy secrets." >&2
  exit 1
fi
set -a
# shellcheck disable=SC1091
. "$ROOT/.env"
set +a

# The secrets the deploy workflow reads as ${{ secrets.NAME }}, seeded per-repo for
# the `gitmote` repo. Fail loud on any missing one — a self-deploy with a blank SCW
# key would only surface deep inside act.
REQUIRED=(
  GITMOTE_REPO_SECRET_GITMOTE__SCW_SECRET_KEY
  GITMOTE_REPO_SECRET_GITMOTE__SCW_ACCESS_KEY
  GITMOTE_REPO_SECRET_GITMOTE__SCW_ORGANIZATION_ID
  GITMOTE_REPO_SECRET_GITMOTE__SCW_PROJECT_ID
  GITMOTE_REPO_SECRET_GITMOTE__SCW_CONTAINER_ID
  GITMOTE_REPO_SECRET_GITMOTE__GHCR_USER
  GITMOTE_REPO_SECRET_GITMOTE__GHCR_TOKEN
)
missing=()
for v in "${REQUIRED[@]}"; do
  [ -n "${!v:-}" ] || missing+=("$v")
done
if [ "${#missing[@]}" -gt 0 ]; then
  echo "ERROR: .env is missing required deploy secrets:" >&2
  printf '  %s\n' "${missing[@]}" >&2
  exit 1
fi

# --- 2. Preflight: act + a reachable Docker/podman daemon ---------------------
command -v act >/dev/null 2>&1 || { echo "ERROR: act not on PATH — run: brew install act" >&2; exit 1; }
if ! git cat-file -e HEAD:.gitmote/workflows/deploy.yml 2>/dev/null; then
  echo "WARN: HEAD has no .gitmote/workflows/deploy.yml — commit it, or the push triggers no deploy." >&2
fi

# act (and the image build) need a live Docker-API socket. Resolve one into
# DOCKER_HOST so act and the runner inherit it instead of defaulting to a socket
# that may not be there. We ask the docker CLI's active context, which covers
# colima (~/.colima/default/docker.sock) and Docker Desktop uniformly.
ping_socket() { # curl liveness of a unix docker socket (catches permission-denied)
  [ -S "$1" ] && curl -fsS --max-time 3 --unix-socket "$1" http://d/_ping >/dev/null 2>&1
}
resolve_docker_host() {
  # An already-set, reachable DOCKER_HOST wins.
  if [ -n "${DOCKER_HOST:-}" ] && ping_socket "${DOCKER_HOST#unix://}"; then return 0; fi
  # The default socket (Docker Desktop, or a colima symlink).
  if ping_socket /var/run/docker.sock; then export DOCKER_HOST="unix:///var/run/docker.sock"; return 0; fi
  # Whatever socket the active docker context points at (colima's, typically).
  if command -v docker >/dev/null 2>&1; then
    local host
    host="$(docker context inspect --format '{{.Endpoints.docker.Host}}' 2>/dev/null || true)"
    if [ -n "$host" ] && ping_socket "${host#unix://}"; then export DOCKER_HOST="$host"; return 0; fi
  fi
  return 1
}
if ! resolve_docker_host; then
  cat >&2 <<'EOF'
ERROR: no reachable Docker daemon — act can't build the image.
       (act connects to DOCKER_HOST, else /var/run/docker.sock.)

Start one, then re-run `make self-deploy`:

  colima (recommended on Apple Silicon — Rosetta-accelerated amd64 builds):
      brew install colima docker docker-buildx
      colima start --vm-type vz --vz-rosetta

  Docker Desktop:
      open -a Docker                              # wait for it to come up
EOF
  exit 1
fi
echo "--- daemon: DOCKER_HOST=$DOCKER_HOST ---"

# --- 3. All-local server config (none secret) ---------------------------------
export GITMOTE_S3_BUCKET="gitmote"
export GITMOTE_S3_ENDPOINT="http://localhost:${S3_PORT}"
export AWS_REGION="us-east-1"
export AWS_ACCESS_KEY_ID="minioadmin"
export AWS_SECRET_ACCESS_KEY="minioadmin"
export GITMOTE_DATA="$DATA"
export GITMOTE_COOKIE_KEY="selfdeploy-cookie-key-not-for-production"
export GITMOTE_URL="http://localhost:${PORT}"
export WORKER_SECRET="selfdeploy-worker-secret-not-for-production"
export GITMOTE_LISTEN_ADDR=":${PORT}"
# The build capability the deploy needs: hand act the host Docker daemon. Safe here
# because this instance hosts only gitmote's own (trusted) repo (docs/ops.md).
export GITMOTE_CI_ALLOW_BUILDS=1

# Free :$PORT from a stale self-deploy server (it must release the writer lease and
# the MinIO volume before the reset below).
if pid="$(lsof -nP -tiTCP:${PORT} -sTCP:LISTEN 2>/dev/null)"; then
  echo "--- stopping process on :${PORT} (pid $pid) ---"
  kill $pid 2>/dev/null || true
  for _ in $(seq 1 20); do
    lsof -nP -tiTCP:${PORT} -sTCP:LISTEN >/dev/null 2>&1 || break
    sleep 0.1
  done
fi

# The self-deploy instance is disposable: always start from a clean slate. This
# removes the whole class of "an interrupted previous run left litestream state
# drifted from the bucket" failures (the txid-mismatch replication loop). It wipes
# ONLY the dedicated gitmote-selfdeploy volume and data-selfdeploy/ dir — nothing
# shared with `make dev`. The just-finished run's state survives until the re-run.
echo "--- resetting throwaway state (data-selfdeploy + its MinIO volume) ---"
"${COMPOSE[@]}" down -v 2>/dev/null || true
rm -rf "$DATA"
mkdir -p "$DATA"

echo "--- starting MinIO (S3 :${S3_PORT}, console :9201) ---"
"${COMPOSE[@]}" up -d minio
"${COMPOSE[@]}" run --rm mc

echo "--- bootstrapping admin + gitmote repo (fresh) ---"
bin/gitmote bootstrap -handle admin -repo gitmote -default-branch master >/dev/null
TOKEN="$(bin/gitmote bootstrap -reissue -handle admin | grep -oE 'gmt_[0-9a-f]+\.[0-9a-f]+' | head -1)"
[ -n "$TOKEN" ] || { echo "ERROR: no token from 'bootstrap -reissue'" >&2; exit 1; }

# --- 4. Start the server in the background, wait for it to serve ---------------
echo "--- starting gitmote on :${PORT} (GITMOTE_CI_ALLOW_BUILDS=1) ---"
bin/gitmote &
SERVER_PID=$!
trap 'kill $SERVER_PID 2>/dev/null || true' EXIT
for _ in $(seq 1 50); do
  curl -fsS -o /dev/null "http://localhost:${PORT}/login" 2>/dev/null && break
  kill -0 $SERVER_PID 2>/dev/null || { echo "ERROR: server exited during startup" >&2; exit 1; }
  sleep 0.2
done

# --- 5. Push HEAD → self-deploy: this is the trigger --------------------------
echo "--- pushing HEAD → self-deploy (triggers the deploy workflow) ---"
git push --force "http://admin:${TOKEN}@localhost:${PORT}/gitmote" HEAD:refs/heads/self-deploy

cat <<EOF

  Self-deploy triggered. The local runner is now building the image and, on
  success, deploying it to gitmote.atmin.net via scw container update.

    Watch:  http://localhost:${PORT}/gitmote   (sign in with the token below)
    token:  $TOKEN

  This first build is amd64 under emulation — expect it to be slow. Ctrl-C stops
  the server (and this script); 'make self-deploy-reset' wipes the throwaway state.

EOF

# Keep the server up so the runner can clone, build, and report back.
wait $SERVER_PID
