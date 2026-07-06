# Build a static gitmote (+ pre-receive hook) and run it beside stock git.
# modernc.org/sqlite is pure Go, so CGO_ENABLED=0 yields a static binary and the
# runtime image needs only git.

# syntax=docker/dockerfile:1
# The build stage runs on the native BUILDPLATFORM and cross-compiles to
# TARGET* (Go is pure-Go/CGO_ENABLED=0), so a multi-arch build never emulates
# the compiler — only the runtime stage's apk runs under QEMU.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=docker
ARG TARGETOS TARGETARCH
# -s -w strips the symbol table and DWARF: ~25% smaller binaries (44M→33M for
# gitmote), which shrinks the image and the cold-start image pull. Panics still
# print stack traces; only gdb/delve attach is lost — irrelevant for prod.
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags "-s -w -X main.version=${VERSION}" -o /out/gitmote ./cmd/gitmote \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags "-s -w" -o /out/gitmote-hook ./cmd/gitmote-hook

# The whole design delegates to `git http-backend`; Alpine ships it in the
# git-daemon package (Debian bundles it into git), so both must be installed.
# Alpine over debian-slim cuts the runtime image from ~150MB to ~20MB of OS+git;
# the static CGO_ENABLED=0 binaries don't care about musl vs glibc.
FROM alpine:3.21
RUN apk add --no-cache git git-daemon ca-certificates
# gitmote resolves the hook as gitmote-hook beside its own executable, so keep
# both in the same directory.
COPY --from=build /out/gitmote /out/gitmote-hook /usr/local/bin/
# db, cache, and socket all live under GITMOTE_DATA; mount one volume at /data to
# persist metadata (also restored from S3 on cold start).
ENV GITMOTE_DATA=/data
RUN mkdir -p /data
EXPOSE 8080
ENTRYPOINT ["gitmote"]
