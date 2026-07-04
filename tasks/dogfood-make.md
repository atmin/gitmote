# Dogfood Make targets — build, run, publish the image

Part of [easy to operate](README.md). Operator ergonomics for the container
lifecycle. Depends on [minimal-env-replica](minimal-env-replica.md) (so
`make prod` restores the *whole* forge from S3) and
[public-registry](public-registry.md) (the image tags).

## Spec

Three targets, distinct from `make build` (which stays the Go binaries):

- `make image` — `docker build` the server (and runner) image, tagged
  `ghcr.io/atmin/gitmote:$(VERSION)` / `…/gitmote-runner:$(VERSION)`.
- `make prod` — run **that image** against the dev MinIO, sharing the `gitmote`
  bucket so it sees the same state as `make dev`, on `:8080`. This is the local
  dogfood of the exact prod deployment — proof the container is portable and
  restores from S3.
- `make publish` — `docker push` the image(s) to GHCR (after `docker login
  ghcr.io`). CI publishes on master; this is the manual / first-time path.

## Current

The Makefile builds Go binaries (`make build`) and runs gitmote **natively** via
`make dev`. There is no way to exercise the actual container image locally, so the
"portable container, localhost is valid" claim is untested outside prod.

## Change

- Add `image` / `prod` / `publish` targets. `make prod` reaches MinIO over the dev
  compose network, not the host port: `--network gitmote-dev_default` (resolve the
  name robustly — `podman compose` may name it differently),
  `GITMOTE_S3_ENDPOINT=http://minio:9000`, `GITMOTE_S3_BUCKET=gitmote`, MinIO creds.
- Reuse the dev MinIO (bring it up if needed).

## Verify

- `make image && make prod` boots the built image against the dev MinIO, shares
  `make dev`'s state (meta + objects, post-replica-task), and serves the UI on
  `:8080` — the portability / restore-from-S3 proof.
- Run **one at a time** with `make dev`: the lease makes the second instance a
  follower that 503s reads (correct single-writer behavior) — verify that's what
  happens, not a crash.
- `make publish` pushes to GHCR (manual login).
