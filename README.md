# gitmote

> *Your own, personal, GitHub.*

gitmote is a git remote that keeps all its state in one S3 bucket. You run it as a
container on your own machine, and only while you need it. The bucket is the
forge, so no server has to stay up.

gitmote has run in production since July 2026. gitmote targets a few repositories
and a few people, not scale.

gitmote provides:

- git over HTTP for clone, fetch and push, with access tokens
- public and private repositories, with read or write access for invited users
- a web UI to browse code and to manage:
  - users and tokens
  - access
  - CI secrets
- CI for `.gitmote/workflows` in GitHub Actions syntax, once you set up a runner
  ([docs/configuration.md](docs/configuration.md#ci))

## Quickstart

You need Docker and an S3-compatible bucket with an access key.

Create a credentials file that only you can read, and open it in an editor:

```sh
mkdir -p ~/.config/gitmote && cd ~/.config/gitmote
touch s3-credentials && chmod 600 s3-credentials
${EDITOR:-nano} s3-credentials
```

Paste the keys into the file. They never pass through your shell, so they stay
out of its history.

```ini
[default]
aws_access_key_id = …
aws_secret_access_key = …
```

Save the following configuration as `compose.yaml`, beside `s3-credentials`:

```yaml
services:
  gitmote:
    image: ghcr.io/atmin/gitmote:master  # tags: :master and :<git-sha>; there is no :latest
    restart: unless-stopped
    ports: ["127.0.0.1:6683:8080"]       # this machine only; 6683 is MOTE on a phone keypad
    environment:
      GITMOTE_S3_BUCKET: my-gitmote-bucket
      GITMOTE_S3_ENDPOINT: https://s3.fr-par.scw.cloud  # non-AWS S3 (Scaleway, MinIO, R2); omit for AWS
      AWS_REGION: fr-par
      AWS_SHARED_CREDENTIALS_FILE: /run/secrets/s3
    secrets: [s3]
    volumes: [gitmote-data:/data]         # keeps the local cache across restarts
secrets:
  s3:
    file: ./s3-credentials
volumes:
  gitmote-data:
```

Start gitmote:

```sh
docker compose up -d
```

On the first start, gitmote creates the user `admin` and prints its access token
in the logs. Read the token with `docker compose logs`, or in the Logs tab of
Docker Desktop. The logs show the token only once, so save it.

Sign in at <http://localhost:6683/login> with the token, and create a repository
on the dashboard. Then clone it:

```sh
git clone http://localhost:6683/<repo>   # user: admin, password: the token
```

Git asks for the token. A credential helper, such as the macOS Keychain or Git
Credential Manager, stores it after the first time. Keep the token out of the
URL, because git saves the URL in `.git/config` as plain text.

## Day to day

Stop gitmote with `docker compose stop`, and start it with `docker compose up -d`.
To keep it always on, leave it running: `restart: unless-stopped` starts it with
Docker. To start Docker Desktop at sign-in, turn on its setting in Settings,
General.

If you lose the token, mint a new one. gitmote stores only a hash of each token,
so it cannot show an old one.

```sh
docker compose stop
docker compose run --rm gitmote bootstrap -reissue
docker compose start
```

To use the forge from another machine, set the same values there as here:

- the bucket
- the endpoint
- the region

Sign in with your token, and do not bootstrap again. With a different bucket,
gitmote starts a new, empty forge.

## How it works

The bucket holds all the state. Git objects are files in it. SQLite holds the
rest:

- the refs
- the users
- the access rules

[s3lite](https://github.com/atmin/s3lite) replicates SQLite to the same bucket.
A push updates its refs in one SQL transaction, so the push is atomic.

The container holds only a cache. Delete the container and its volume, and the
next start restores the forge from the bucket.

One instance writes at a time, and a lease in the bucket enforces this. An
instance releases the lease after 5 seconds without requests. So a container on
your laptop and one in the cloud can share a bucket. A request that reaches one
instance while the other is busy gets a `503`. Retry the request.

## Documentation

- [docs/configuration.md](docs/configuration.md): read it to set the env vars,
  turn on CI, or recover the admin token.
- [docs/deploy-scaleway.md](docs/deploy-scaleway.md): read it to run gitmote in
  the cloud.
- [docs/](docs/README.md): read it to find the design docs.
- [CONTRIBUTING.md](CONTRIBUTING.md): read it to build, test or change gitmote.

---

<sub>`git push gitmote` — reach out and touch faith. 🎶</sub>
