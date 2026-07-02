# Build a static gitmote (+ pre-receive hook) and run it beside stock git.
# modernc.org/sqlite is pure Go, so CGO_ENABLED=0 yields a static binary and the
# runtime image needs only git.

# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=docker
RUN CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION}" -o /out/gitmote ./cmd/gitmote \
 && CGO_ENABLED=0 go build -o /out/gitmote-hook ./cmd/gitmote-hook

# Debian's git package includes git-http-backend (Alpine's splits it out); the
# whole design delegates to `git http-backend`, so it must be present.
FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates \
 && rm -rf /var/lib/apt/lists/*
# gitmote resolves the hook as gitmote-hook beside its own executable, so keep
# both in the same directory.
COPY --from=build /out/gitmote /out/gitmote-hook /usr/local/bin/
RUN mkdir -p /data /cache
EXPOSE 8080
ENTRYPOINT ["gitmote"]
