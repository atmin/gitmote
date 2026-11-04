# Contributing

Entry point for anyone — human or agent — working on gitmote. Read it first.

The repo is **documentation-first**. The design lives in
[docs/architecture/](docs/architecture/) and is the source of truth for *what*
the system does and *why* it's shaped this way. If code and docs disagree, fix
one of them in the same change — never leave the divergence silently.

## Layout

```
docs/
  architecture/   The design and its decisions — source of truth
  notes/          Open design questions, one concern per file
  evolution/      Speculative future directions — ideas, not commitments
  ops.md          Canonical infra doc — deploy, env, the single-writer rule
tasks/            The frontier — active/upcoming work; deleted once landed
```

## Run it locally

You need Docker with Docker Compose.

`make dev` builds the binaries and starts an S3 server in a container on port
9100. The server is Silo, a fork of MinIO. Then `make dev` runs gitmote natively
on port 6683. It creates the user `admin` and the repository `gitmote`, and prints
a fresh token. The metadata database and the object cache persist in `data/`, so
repositories survive a restart.

```sh
make dev        # prints the token, the clone URL and the UI URL
make dev-reset  # deletes the S3 volume and data/
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

## Commit messages

[Conventional Commits](https://www.conventionalcommits.org/), no scope.

```
type: short imperative description

Optional body explaining why, not what.
```

**Types:** `feat` `fix` `refactor` `perf` `ci` `chore` `docs` `revert`
**Breaking change:** append `!` — `feat!:`.

- Subject: lowercase, no trailing period, imperative mood ("add", not "added").
- Em dash for a subtitle when a bare type isn't enough context:
  `feat: pre-receive hook — RPC the CAS back to the parent`.
- Body: optional, blank-line separated. Explain *why*, not *what* — one to three
  sentences. Omit when the subject says it all.
- No trailers. Leave out `Co-Authored-By` and "Generated with" lines, also when
  an agent writes the commit.

## Testing — test everything

**"Safe" is the top priority** (see [safety](docs/architecture/safety.md)), so
tests are not optional:

- Cover the **golden path *and* the failure path** for every change. A
  rejection is not an afterthought — a non-fast-forward that isn't refused is a
  bug as real as a lost commit.
- The **safety invariants get explicit tests**: the ref CAS under concurrent
  pushes, content-before-pointer ordering, atomic multi-ref push. A change to
  the write path proves the invariant still holds.
- Integration tests drive **real `git`** (clone / fetch / push) against the
  server — the protocol is the contract, not our idea of it.

Every change passes, in order — **format → lint → test**:

```
gofmt / goimports    # format
golangci-lint run    # lint (includes vet)
go test ./...        # test
```

`make fmt` formats the code, and `make all` runs lint, test and build. CI runs
`make all` and the end-to-end suites.

## Tasks

`tasks/` is the **frontier** — one file per intent-to-implement unit of work,
**deleted once it lands** (commits and git history are the record; tasks are not
a changelog). Each file follows:

```
## Spec      What the change must achieve (link the docs/architecture/ section)
## Current   How it works today (or "nothing yet")
## Change    The plan — files, approach, trade-offs
## Verify    How we'll know it's done — the tests/checks that must pass
```

Keep `tasks/README.md` a forward-looking, one-line-per-item list of
active/upcoming work. Never let it accrete "landed X…" prose.

## Docs

- **Architecture** ([docs/architecture/](docs/architecture/)) — the design *and
  its rationale, inline*. There are no ADRs: a decision and its "why" live in
  the relevant architecture doc (the existing "Why it looks like this" sections
  are the pattern). It is current-state — supersede by editing it and keeping it
  correct, not by appending history.
- **Notes** ([docs/notes/](docs/notes/)) — one open question per file; each
  feeds a future task or an architecture change.
- **Evolution** ([docs/evolution/](docs/evolution/)) — speculative; never cite
  as a spec.
- Backticks for paths, env vars, and identifiers; Mermaid for sequence flows.
