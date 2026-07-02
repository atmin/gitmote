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

The Go server, Makefile, and CI arrive as code lands.

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

A Makefile will front these as the single source of truth once code lands; CI
runs the same targets, so green locally = green in CI.

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
