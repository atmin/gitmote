# Single source of truth for the gates: format → lint → test (CONTRIBUTING.md).
# CI runs the same targets, so green locally = green in CI.

GOLANGCI_LINT := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2
GOIMPORTS     := go run golang.org/x/tools/cmd/goimports@v0.47.0

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: all fmt lint test build dev dev-reset e2e-local e2e-restore

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
