# Content routing — tree/blob/raw + ref-in-path

Part of the **URLs** epic ([README](README.md)); implements the content-addressing
section of [../docs/architecture/urls.md](../docs/architecture/urls.md). Needs the
flat namespace ([urls-namespace-access.md](urls-namespace-access.md)).

## Spec

- Content verbs under `/<repo>/`: **`tree`** (unified — dir → listing, file →
  view), **`blob`** (explicit file; a dir **301s to `tree`**), **`raw`** (bytes).
- The **ref is in the path**, resolved **greedily**: the longest leading
  path-prefix that names a real branch/tag. Cross-type prefix collision (a branch
  and a tag) resolves branch-first. An omitted ref (the `/<repo>` landing) means
  the default branch.
- Also `commits`/`commit`/`refs`/`runs` under `/<repo>/`; **no `?ref=` query
  param**, **no `/-/` marker**.

## Current

Browse is `/browse/<repo>/-/<action>/<arg>` with the **ref as a query parameter**
([`browse.go`](../internal/webui/browse.go)): the path is split on `/-/` to find
where the (slashed) repo name ends, `action` is `tree|blob|raw|commits|commit|
runs|run`, and `resolve` reads `ref` from `r.URL.Query()` else the default branch.
`tree` and `blob` are separate renderers; a relative link loses `?ref=` and can
hit the wrong verb → "empty tree". This is the concrete breakage the epic fixes.

## Change

- **Routes** ([`webui.go`](../internal/webui/webui.go)): register
  `GET /{repo}/tree/{rest...}`, `.../blob/...`, `.../raw/...`, `.../commits/...`,
  `.../commit/{sha}`, `.../refs`, `.../runs`, `.../runs/{id}`, and the bare
  `GET /{repo}` landing. Drop the `/browse/` prefix and the `/-/` split (repo is
  one segment now). **Mux seam:** these now share the mux with the git smart-HTTP
  catch-all — register **only these enumerated verbs**, never a broad
  `/{repo}/{rest...}`, so git's `info/refs` / `*-pack` fall through to the git
  handler (see [../docs/architecture/urls.md](../docs/architecture/urls.md) →
  Implementation seams).
- **Ref-in-path** ([`browse.go`](../internal/webui/browse.go) `resolve`): replace
  the `?ref` read with greedy resolution against the repo's ref list —
  `meta.ListRefs` ([`refs.go`](../internal/meta/refs.go), refs are source of
  truth) for branches, plus tags. Longest-prefix match; branch-over-tag on a tie;
  default branch when the tail has no ref (landing). A path that resolves to no
  ref → 404.
- **tree unified**: one handler renders a listing for a tree entry and the file
  view for a blob, chosen by the tree lookup `resolve` already does.
- **blob explicit**: render the file; if the entry is a tree, **301 to the `tree`
  URL** (same ref+path).
- **raw**: bytes as today, file-only (dir → 404).
- **Link generation** in the browse templates emits `blob` for files and `tree`
  for directories (it has the tree entry's type), with both verbs tolerant per
  above. (Markdown-body links are handled in
  [urls-markdown-links.md](urls-markdown-links.md).)

## Verify

- `/<repo>` → landing at default branch; `/<repo>/tree/main/docs` → listing;
  `/<repo>/tree/main/docs/readme.md` → file view.
- `/<repo>/blob/main/readme.md` → file view; `/<repo>/blob/main/docs` → **301** to
  `/<repo>/tree/main/docs`; `/<repo>/raw/main/x` → bytes; `/raw` on a dir → 404.
- **Slashed branch** `feature/x`: `/<repo>/tree/feature/x/README.md` resolves ref
  `feature/x`, path `README.md`.
- Unknown ref or path → 404 (not a 500, not an "empty tree").
- `gofmt`/`golangci-lint`/`go test ./...` clean; golden **and** failure paths
  (bad ref, blob-on-dir redirect, greedy tie-break).
