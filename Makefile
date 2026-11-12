# This file is the single source of truth for the gates (CONTRIBUTING.md).
# `make fmt` formats the code. `make all` runs lint, test and build.
# CI runs the same targets, so a pass locally means a pass in CI.

GOLANGCI_LINT := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2
GOIMPORTS     := go run golang.org/x/tools/cmd/goimports@v0.47.0

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# The public GHCR server image. CI publishes it on master. The targets below
# build, run and publish it locally (docs/deploy-scaleway.md).
# The CI runner image (ghcr.io/atmin/gitmote-runner) is amd64 only, and its
# build runs an act self-test. So it can't build under local emulation on an
# arm64 host. CI publishes it (.github/workflows/publish-runner.yml), or you
# follow the manual amd64 path in docs/deploy-scaleway.md.
IMAGE_SERVER := ghcr.io/atmin/gitmote

# The host port for `make prod`. The default is 8080, the same port as in prod.
# So the container runs beside a native `make dev` on 6683. The two share the
# bucket and take turns as the writer. Each one yields the lease when idle
# (docs/configuration.md).
PORT ?= 8080

# The dev compose project (compose/dev.yml sets name: gitmote-dev). `make prod`
# joins the network of this project, so the container reaches MinIO at
# minio:9000.
DEV_COMPOSE := docker compose -p gitmote-dev --project-directory . -f compose/dev.yml

.PHONY: all fmt lint test build dev dev-reset self-deploy self-deploy-reset e2e-local e2e-restore e2e-handoff e2e-build image prod publish

all: lint test build

fmt:
	gofmt -w .
	$(GOIMPORTS) -w .

lint:
	$(GOLANGCI_LINT) run

test:
	go test ./...

build:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/gitmote ./cmd/gitmote
	go build -o bin/gitmote-hook ./cmd/gitmote-hook
	go build -o bin/gitmote-runner ./cmd/gitmote-runner

# Local dev runs MinIO in a container (S3 on :9100) and gitmote natively on
# :6683. The bootstrapped admin, token and repo persist under data/dev/ across
# restarts. The first run prints the token and the clone URL. Ctrl-C stops the
# server. See scripts/dev.sh. Requires docker and docker compose.
dev: build
	./scripts/dev.sh

# Wipe all local dev state: the MinIO volume, and the persisted DB, cache and
# token.
dev-reset:
	$(DEV_COMPOSE) down -v 2>/dev/null || true
	rm -rf data/dev

# Break-glass self-deploy: gitmote builds and deploys itself to gitmote.atmin.net
# through its own local runner. The secrets come from a gitignored .env, not the
# Secrets UI. The target runs a throwaway instance on :8081 with its own MinIO
# (:9300). Then it pushes HEAD to the `self-deploy` branch. GitHub Actions only
# publishes the image. Requires act and docker. The deploy workflow needs
# GITMOTE_CI_ALLOW_BUILDS, and the script sets it. See docs/deploy-scaleway.md.
# First copy .env.example to .env and fill it in.
self-deploy: build
	./scripts/self-deploy.sh

# Wipe the throwaway self-deploy state: its MinIO volume and its data directory.
self-deploy-reset:
	docker compose -p gitmote-selfdeploy --project-directory . -f compose/selfdeploy.yml down -v 2>/dev/null || true
	rm -rf data/selfdeploy

# End-to-end test: gitmote hosts itself against MinIO in Docker. Integration
# tests drive real git (CONTRIBUTING.md). Requires docker and docker compose.
e2e-local:
	./scripts/e2e-local.sh

# Litestream cold-start test. It does these steps:
#   - Push with metadata replication on.
#   - Wipe the DB volume.
#   - Confirm that the repo still clones, with refs restored from S3.
# It tests the production cold-start path locally. Requires docker and docker
# compose.
e2e-restore:
	./scripts/e2e-restore.sh

# Two live instances on one storage root take turns as the writer:
#   - A follower serves reads while the other instance holds the lease.
#   - Each instance yields the lease when idle.
#   - Each instance serves the pushes that the other received.
#   - A warm cache never advertises a ref that the other instance moved or
#     deleted.
# Requires docker and docker compose.
e2e-handoff:
	./scripts/e2e-handoff.sh

# Test that GITMOTE_CI_ALLOW_BUILDS gates real image builds. It runs act in a
# privileged DinD container with its own rootful daemon. With the socket
# suppressed, the build fails. With the socket mounted, the build succeeds.
# Skips when no daemon runs. Requires docker or podman.
e2e-build:
	./scripts/e2e-build.sh

# Build the server container image, tagged with VERSION. This differs from
# `make build`, which builds the Go binaries. CI publishes this exact image, and
# `make prod` runs it. The runner image is amd64 only and built by CI (see the
# IMAGE_SERVER note above).
image:
	docker build -t $(IMAGE_SERVER):$(VERSION) --build-arg VERSION=$(VERSION) .

# Run the built server image against the dev MinIO. It shares the `gitmote`
# bucket, so it restores the same forge state as `make dev`. This runs the exact
# prod deployment locally: a portable container that restores from S3. The env
# sets no cookie key or worker secret. Both are generated and persist in meta,
# exactly as in prod. Serves the UI on :$(PORT). Requires docker and docker
# compose. Run `make dev-reset` to wipe the state.
prod: image
	$(DEV_COMPOSE) up -d minio
	$(DEV_COMPOSE) run --rm mc
	@net=$$(docker network ls --format '{{.Name}}' | grep -m1 gitmote-dev || echo gitmote-dev_default); \
	echo "--- running $(IMAGE_SERVER):$(VERSION) against dev MinIO (network $$net) on :$(PORT) ---"; \
	docker run --rm -p $(PORT):8080 --network "$$net" \
	  -e GITMOTE_S3_BUCKET=gitmote \
	  -e GITMOTE_S3_ENDPOINT=http://minio:9000 \
	  -e AWS_REGION=us-east-1 \
	  -e AWS_ACCESS_KEY_ID=minioadmin \
	  -e AWS_SECRET_ACCESS_KEY=minioadmin \
	  $(IMAGE_SERVER):$(VERSION)

# Push the built server image to GHCR. This is the manual, break-glass path.
# CI publishes on master. First run `docker login ghcr.io` with a PAT that has
# write:packages. The runner image publishes through CI or through the amd64
# path in docs/deploy-scaleway.md.
publish: image
	docker push $(IMAGE_SERVER):$(VERSION)
