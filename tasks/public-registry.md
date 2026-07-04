# Public GHCR images; deploy from public

Part of [easy to operate](README.md). Make the image runnable "from the
internet" and stop hand-pushing the runner. Independent of the env-shrink tasks
(can proceed in parallel).

## Spec

`gitmote` and `gitmote-runner` are published **publicly** to GHCR
(`ghcr.io/atmin/gitmote`, `…/gitmote-runner`). `docker run ghcr.io/atmin/gitmote`
works anywhere. Scaleway deploys by pulling the public image (its Serverless
Container + the CI Job definition both reference GHCR). This is also the pragmatic
self-deploy path: build where a real daemon exists (GitHub Actions, or locally),
push public, and "deploy" is a retag + `scw container update` — no in-Job build
(the sandbox can't).

## Current

`.github/workflows/ci.yml` builds `rg.fr-par.scw.cloud/atmin/gitmote` (private
Scaleway registry) and `scw container update`s it. The **runner** image is built
and pushed **by hand** ([ci.md](../docs/architecture/ci.md)); the Job definition
points at `rg.fr-par.scw.cloud/atmin/gitmote-runner:master`.

## Change

- Deploy job: build + push `ghcr.io/atmin/gitmote:{sha,master}` (public), point
  `scw container update image=ghcr.io/atmin/gitmote:<sha>`.
- Add a **runner image** build+push to CI (on `Dockerfile.runner` change / on
  release), publishing `ghcr.io/atmin/gitmote-runner` — removes the manual push.
- Update the `scw jobs definition` image to the GHCR runner path (one-time, doc
  in ops.md).

## Verify

- A bare `docker run --rm -e GITMOTE_S3_BUCKET=… -e AWS_… ghcr.io/atmin/gitmote`
  boots and serves `/healthz`.
- A push to master builds + pushes GHCR and Scaleway redeploys from it (container
  image SHA flips to the GHCR ref).
- A cloud CI run pulls the GHCR runner image and reports green.

## Gotcha

New GHCR packages default to **private** — the package must be flipped to
**public** once by hand (GitHub → package settings) before Scaleway can pull it
anonymously. Otherwise the first deploy fails on an image pull the code can't
diagnose. Publishing from Actions needs `packages: write` (built-in
`GITHUB_TOKEN`).
