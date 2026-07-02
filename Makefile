# Single source of truth for the gates: format → lint → test (CONTRIBUTING.md).
# CI runs the same targets, so green locally = green in CI.

GOLANGCI_LINT := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2
GOIMPORTS     := go run golang.org/x/tools/cmd/goimports@v0.47.0

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: all fmt lint test build

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
