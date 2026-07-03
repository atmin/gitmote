# Browse a repo in the web UI

## Spec

Read-only repo browsing in the existing web UI, admin-session gated (same gate as
the rest of `/ui`). At an arbitrary ref (branch, tag, or hash) a user can:

- **Tree** — list a directory (name, type, mode, size).
- **Blob** — view a file's raw contents; download it raw.
- **Commits** — the commit log for a ref (optionally scoped to a path).
- **Commit** — a single commit with its diff.
- **Ref switcher** — pick among the repo's branches/tags.

Deliberately **out of scope** (later, if the lack is felt): syntax highlighting,
markdown/README rendering, blame, search, line-level permalinks, submodule follow.
Everything here is cheap `git` plumbing against the already-materialized repo — no
new storage, no new object-hydration work.

## Current

The web UI ([internal/webui/webui.go](../internal/webui/webui.go)) is
admin-management only — repos, users, tokens, ACLs — all behind
[`requireAdmin`](../internal/webui/webui.go#L113). Its `Handler` holds a
`*meta.Metadata` but **no `*repo.Materializer`**, so it can't reach objects.

Object access already exists on the git path:
[`Materializer.Materialize(ctx, name)`](../internal/repo/materialize.go#L49)
full-hydrates the bare repo (refs from s3lite, every object from the store) and
returns its on-disk dir — this runs in prod on every clone. After it runs, the
dir is an ordinary bare repo, readable with git plumbing. The package's git
exec helper [`runGit`](../internal/repo/materialize.go#L168) only returns an
error, though — it does **not** capture stdout, which browsing needs.

Refs/branches are already listable without git:
[`meta.ListRefs(repoID)`](../internal/meta/refs.go#L45) → `[]meta.Ref`,
[`GetRepo`](../internal/meta/repos.go#L55) gives `DefaultBranch`.

## Change

**1. A read API in the `repo` package** — a small reader over a materialized
dir, backed by a new stdout-capturing git exec helper (sibling to `runGit`).
Methods, each a thin plumbing call:

- `ResolveRef(ctx, dir, ref) (sha string, err error)` — `git rev-parse --verify
  <ref>^{commit}`. **Reject a `ref` starting with `-`** (flag injection) and
  return a not-found sentinel on unknown ref.
- `Tree(ctx, dir, sha, path) ([]TreeEntry, err)` — `git ls-tree --long <sha> --
  <path>`; entry = {mode, type (blob/tree), sha, size, name}.
- `Blob(ctx, dir, sha, path) (content []byte, size int64, binary bool, err)` —
  `cat-file -s` for size, stream `cat-file blob <sha>:<path>`; flag binary by a
  NUL-byte sniff of the leading bytes.
- `Log(ctx, dir, sha, path string, limit int) ([]Commit, err)` — `git log
  --format=<record-sep> -n <limit+1> <sha> -- <path>`; report + drop the `+1`
  overflow as a "more" marker (**no silent truncation** — surface it in the UI).
- `Show(ctx, dir, sha) (Commit, diff string, err)` — `git show --format=…
  --patch <sha>` (use `--root` so the initial commit diffs).

Branches/tags for the switcher come from `meta.ListRefs` — no git needed.

**2. Wire the `Materializer` into the webui `Handler`** — add the dep to the
struct and `New(...)`; the caller in [cmd/gitmote/main.go](../cmd/gitmote/main.go)
already builds one for githttp, pass the same instance.

**3. Routes + a manual path split.** Repo names contain slashes
(`atmin/atmin.net`) and Go's `{repo...}` wildcard must be the final segment, so it
can't carry a suffix. Mirror [`parseGitPath`](../internal/githttp/backend.go#L298):
register one subtree handler and split manually on a `/-/` marker (GitLab's
convention) into `repoName` and an action tail. **`ref` is a query parameter**
(defaults to `DefaultBranch`) so a slashed branch (`feature/x`) needs no
disambiguation from the path:

```
GET /browse/{repo}/-/tree/{path}?ref=…      dir listing
GET /browse/{repo}/-/blob/{path}?ref=…      file view
GET /browse/{repo}/-/raw/{path}?ref=…       raw bytes (Content-Disposition, sniffed type)
GET /browse/{repo}/-/commits?ref=…&path=…   log
GET /browse/{repo}/-/commit/{sha}           single commit + diff
```

All behind `requireAdmin`. Each handler: `Materialize(repo)` → `ResolveRef` →
the relevant reader call → render. `meta.ErrNotFound` (repo) and an
unknown-ref/path → 404.

**4. Templates** (reuse [layout.html](../internal/webui/templates/layout.html),
plain HTML, no highlighting): `browse_tree.html` (breadcrumb + ref switcher +
entry table, dirs and blobs linked), `browse_blob.html` (metadata + `<pre>` for
text / "binary — download" for binary + raw link), `browse_commits.html` (log
rows linking to each commit, "more" marker when capped), `browse_commit.html`
(commit metadata + `<pre>` diff).

**Safety / notes:**

- **Path traversal / injection:** repo name is already validated by
  `repoDir`; additionally reject a `path` that is absolute or contains `..`, and
  a `ref`/`sha` starting with `-`. Always pass `--` before pathspecs.
- **Any hash browses:** `hydrateObjects` copies the *whole* store prefix, so
  every object is present regardless of reachability — an arbitrary valid hash
  resolves. (A hash for objects never pushed simply 404s.)
- **Cost:** browse full-hydrates like a clone — fine at current scale, same
  policy as everywhere else (object-hydration.md); not a regression.
- **Raw endpoint** sets `Content-Type` from a sniff and
  `Content-Disposition: attachment`; it streams, it does not buffer the whole
  blob.

## Verify

- **Reader unit tests** (`internal/repo`): build a fixture bare repo in-test
  (a couple commits, a subdir, a binary blob, a tag), then assert `ResolveRef`
  (branch, tag, hash, and a `-`-prefixed ref → rejected), `Tree` entries,
  `Blob` (text vs binary flag + size), `Log` (ordering + the "more" overflow
  marker at the cap), `Show` (diff present, initial commit diffs via `--root`).
- **webui route tests** — golden **and** failure paths per
  [CONTRIBUTING](../CONTRIBUTING.md): unauth request → redirected/denied by
  `requireAdmin`; tree/blob/commits/commit render for a materialized repo; ref
  switcher lists `meta.ListRefs`; raw download has the attachment header and
  correct bytes; unknown repo → 404, unknown ref → 404, `..` in path → rejected;
  binary blob renders the download affordance, not garbage.
- **Regression:** `make e2e-local` + `make e2e-restore` stay green (browse
  doesn't touch the write path); `gofmt`/`golangci-lint`/`go test ./...` clean.
