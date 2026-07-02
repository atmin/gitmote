# 01 — Project scaffold & gates

Depends on: nothing.

## Spec

Establish the build/test/lint gates promised in
[CONTRIBUTING.md](../CONTRIBUTING.md) so every later task can land green, plus a
minimal server binary that starts, reports version/health, and shuts down
cleanly.

## Current

Docs only — no `go.mod`, no code, no Makefile.

## Change

- `go.mod` (module `github.com/atmin/gitmote`, Go pinned to a recent stable).
- `cmd/gitmote/main.go` — env/flag config, structured logging, an HTTP server
  with `GET /healthz` and `GET /version`, graceful shutdown on SIGINT/SIGTERM.
- `Makefile` — the single source of truth: `fmt` (gofmt/goimports), `lint`
  (golangci-lint, includes vet), `test` (`go test ./...`), `build`, and
  `all = lint test build`.
- `.golangci.yml`; `.github/workflows/ci.yml` calling the same Make targets;
  `scripts/pre-commit` running `make fmt lint test`.

## Verify

- `make all` is green from a clean checkout; CI runs the Make targets and passes.
- `go run ./cmd/gitmote` serves `200` on `/healthz` — asserted with an
  `httptest` unit test.
- Non-breaking: this is the baseline; nothing to regress.
