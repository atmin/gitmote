# Build a static gitmote and its pre-receive hook, and run them beside stock git.
# modernc.org/sqlite is pure Go, so CGO_ENABLED=0 yields a static binary and the
# runtime image needs only git.

# syntax=docker/dockerfile:1
# The build stage runs on the native BUILDPLATFORM and cross-compiles to
# TARGET*, because the build is pure Go with CGO_ENABLED=0. So a multi-arch
# build never emulates the compiler. Only apk in the runtime stage runs under
# QEMU.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=docker
ARG TARGETOS TARGETARCH
# -s -w strips the symbol table and DWARF. The binaries are about 25% smaller
# (44M to 33M for gitmote), which shrinks the image and the cold-start image
# pull. Panics still print stack traces. Only the attach of gdb or delve is
# lost, and prod does not need it.
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags "-s -w -X main.version=${VERSION}" -o /out/gitmote ./cmd/gitmote \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags "-s -w" -o /out/gitmote-hook ./cmd/gitmote-hook

# The whole design delegates to `git http-backend`. Alpine ships it in the
# git-daemon package, while Debian bundles it into git. So both packages must be
# installed. Alpine instead of debian-slim cuts the OS and git in the runtime
# image from about 150MB to about 20MB. The static CGO_ENABLED=0 binaries work
# the same with musl or glibc.
FROM alpine:3.21
RUN apk add --no-cache git git-daemon ca-certificates
# gitmote resolves the hook as gitmote-hook beside its own executable, so keep
# both in the same directory.
COPY --from=build /out/gitmote /out/gitmote-hook /usr/local/bin/
# The db, the cache and the socket all live under GITMOTE_DATA. Mount one volume
# at /data to persist metadata. gitmote also restores metadata from S3 on a cold
# start.
ENV GITMOTE_DATA=/data
RUN mkdir -p /data
EXPOSE 8080
ENTRYPOINT ["gitmote"]
