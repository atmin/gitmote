# Single source of truth for the gates: format → lint → test (CONTRIBUTING.md).
# CI runs the same targets, so green locally = green in CI.

GOLANGCI_LINT := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2
GOIMPORTS     := go run golang.org/x/tools/cmd/goimports@v0.47.0

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# Public GHCR server image (CI publishes it on master; the targets below are the
# local build/run/publish path — see docs/ops.md). The CI *runner* image
# (ghcr.io/atmin/gitmote-runner) is amd64-only and self-tests act at build, so it
# can't build under local emulation on an arm64 host — it's published by CI
# (.github/workflows/publish-runner.yml) or by the manual amd64 path in ops.md.
IMAGE_SERVER := ghcr.io/atmin/gitmote

# Host port for `make prod`. Defaults to the prod-parity 8080; override to run the
# container alongside a native `make dev` (which owns 8080) — the shared bucket's
# writer lease then makes whichever starts second a read-only follower.
PORT ?= 8080

# Dev compose project (docker-compose.dev.yml sets name: gitmote-dev). `make prod`
# joins this project's network so the container reaches MinIO at minio:9000.
DEV_COMPOSE := docker compose -p gitmote-dev -f docker-compose.dev.yml

.PHONY: all fmt lint test build dev dev-reset e2e-local e2e-restore image prod publish

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

# Local dev: MinIO in a container (S3 :9100) + gitmote run natively on :8080,
# with a bootstrapped admin/token/repo persisted under data/ across restarts.
# First run prints the token and clone URL; Ctrl-C stops the server. See
# scripts/dev.sh. Requires docker + docker compose.
dev: build
	./scripts/dev.sh

# Wipe all local dev state: the MinIO volume and the persisted DB/cache/token.
dev-reset:
	docker compose -p gitmote-dev -f docker-compose.dev.yml down -v 2>/dev/null || true
	rm -rf data

# End-to-end: gitmote hosts itself against MinIO in Docker (CONTRIBUTING.md —
# integration tests drive real git). Requires docker + docker compose.
e2e-local:
	./scripts/e2e-local.sh

# Litestream cold-start proof: push with metadata replication on, wipe the DB
# volume, and confirm the repo still clones (refs restored from S3). De-risks the
# production cold-start path locally. Requires docker + docker compose.
e2e-restore:
	./scripts/e2e-restore.sh

# Build the actual server container image (distinct from `make build`, the Go
# binaries), tagged with VERSION — the exact image CI publishes and `make prod`
# runs. (The runner image is CI/amd64-only; see the IMAGE_SERVER note above.)
image:
	docker build -t $(IMAGE_SERVER):$(VERSION) --build-arg VERSION=$(VERSION) .

# Run the built server image against the dev MinIO, sharing the `gitmote` bucket so
# it restores the same forge state as `make dev` — the local dogfood of the exact
# prod deployment (portable container, restore-from-S3). No cookie key / worker
# secret env: they auto-generate and persist in meta, exactly as in prod. Serves
# the UI on :$(PORT). Requires docker + docker compose; run `make dev-reset` to wipe.
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

# Push the built server image to GHCR — the manual / break-glass path (CI
# publishes on master). Requires `docker login ghcr.io` first (a PAT with
# write:packages). The runner image publishes via CI / the ops.md amd64 path.
publish: image
	docker push $(IMAGE_SERVER):$(VERSION)
