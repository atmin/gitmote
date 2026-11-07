# Contributing

Read this first, whether you are a person or an agent working on gitmote.

The docs lead the code. [docs/architecture/](docs/architecture/) holds the design:
what gitmote does, and why it has this shape. When code and docs disagree, fix one
of them in the same change.

## Layout

```
docs/
  README.md            The index: which doc to read for which task
  configuration.md     Every deployment: env vars, single writer, CI, first admin
  deploy-scaleway.md   One deployment: Scaleway, GitHub Actions, self-deploy
  architecture/        The design and its decisions, the source of truth
```

## Run it locally

You need Docker with Docker Compose.

`make dev` builds the binaries and starts an S3 server in a container on port
9100. The server is Silo, a fork of MinIO. Then `make dev` runs gitmote natively
on port 6683. It creates the user `admin` and the repository `gitmote`, and prints
a fresh token. The metadata database and the object cache persist in `data/dev/`, so
repositories survive a restart.

```sh
make dev        # prints the token, the clone URL and the UI URL
make dev-reset  # deletes the S3 volume and data/dev/
```

`make dev` also runs CI. A push of a repository with `.gitmote/workflows/*.yml`
starts a run. `gitmote-runner` executes the run as a local process with
[act](https://github.com/nektos/act), on the same Docker daemon. Install act
first, for example with `brew install act`. gitmote reads `.gitmote/workflows`
and never `.github/workflows`, so a repository mirrored to GitHub runs each
workflow once.

The end-to-end suites run gitmote in a container against the S3 server, with real
git:

- `make e2e-local` pushes this working tree, clones it back, recreates the
  container, and clones again from the bucket.
- `make e2e-restore` deletes the metadata database and checks that a clone works
  after the restore from S3.
- `make e2e-handoff` runs two instances on one bucket and checks that they take
  turns as the writer.
- `make e2e-build` checks that `GITMOTE_CI_ALLOW_BUILDS` gates image builds in
  CI. It skips when no Docker daemon runs.

## Tests

Safety comes first ([safety.md](docs/architecture/safety.md)), so every change
comes with tests:

- Test the success path and the failure path. A rejection matters as much as a
  success: a non-fast-forward push that gitmote accepts is as bad as a lost
  commit.
- Test the safety invariants explicitly: the ref compare-and-swap under
  concurrent pushes, the content-before-pointer order, and the atomic multi-ref
  push. A change to the write path proves that each invariant still holds.
- Drive real `git` (clone, fetch, push) against the server in integration tests.
  The protocol is the contract.

Every change passes these checks, in this order:

```
gofmt / goimports    # format
golangci-lint run    # lint, including vet
go test ./...        # test
```

`make fmt` formats the code, and `make all` runs lint, test and build. CI runs
`make all` and the end-to-end suites.

## Commit messages

Use [Conventional Commits](https://www.conventionalcommits.org/), with no scope:

```
type: short imperative description

Optional body explaining why, not what.
```

- Types: `feat`, `fix`, `refactor`, `perf`, `ci`, `chore`, `docs`, `revert`. For a
  breaking change, append `!`, as in `feat!:`.
- Write the subject in lowercase and the imperative ("add", not "added"), with no
  period at the end.
- When the type alone gives too little context, add a subtitle after an em dash:
  `feat: pre-receive hook — RPC the CAS back to the parent`.
- The body is optional. Separate it with a blank line, and explain why in one to
  three sentences. Leave it out when the subject says enough.
- Add no trailers. Leave out `Co-Authored-By` and "Generated with" lines, also
  when an agent writes the commit.

## Docs

- [docs/architecture/](docs/architecture/) holds the design and its reasons. A
  decision and its reason live in the architecture doc they affect; there are no
  separate decision records. To change a decision, edit the doc so that it stays
  true. Do not append history.
- The repo holds only what is true now. It holds no plans, roadmap, open
  questions or ideas.

Write every doc for a reader with no context, who may read in a second language:

- Put one idea in each sentence, and keep a sentence to 20 words or fewer.
- Use plain, literal words: no idioms, hype, emoji or arrows in prose.
- State the current truth only. Git holds the history.
- Use a list for three or more parallel items, and keep a list to five items.
- Make each index line say when to read a file, not what is inside it.

Put paths, env vars and identifiers in backticks. Draw sequence flows in Mermaid.
