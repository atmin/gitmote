# Markdown link & embed rewriting

Part of the **URLs** epic ([README](README.md)); implements the "relative links in
rendered content" section of
[../docs/architecture/urls.md](../docs/architecture/urls.md). Needs content
routing ([urls-content-routing.md](urls-content-routing.md)) — it rewrites links
*into* those verbs.

## Spec

Rendered markdown resolves in-repo relative links correctly, preserving the ref:

- **Embedded assets** (`![](img.png)`, other embeds) → `/<repo>/raw/<ref>/<path>`
  (bytes, so the image actually renders).
- **Navigation links** (`[x](./doc.md)`, `[y](../sub/)`) → the precise verb:
  `blob` for a file, `tree` for a directory (decided by a tree lookup), with the
  current ref baked in.
- **Untouched:** absolute URLs, external `http(s)`/`mailto:`, and in-page anchors
  (`#section`).

## Current

The markdown renderer ([`internal/render`](../internal/render)) emits links
verbatim. A relative link therefore resolves against the *browser's* current URL,
which (pre-epic) carries `?ref=` in the query — dropped on a relative navigation —
and can't distinguish tree from blob, so an in-repo link lands on "empty tree".
Even after content routing puts the ref in the path, embedded images still need
the `raw` verb, which a bare relative link can't express — so rewriting is
required regardless.

## Change

- Give the render/browse seam the context to rewrite: the repo name, the resolved
  ref, the current file's directory, and a **tree-stat** callback (is `<path>` a
  blob, a tree, or absent?) — the materialized repo already backs this
  ([`internal/repo`](../internal/repo)).
- In the markdown pipeline ([`internal/render`](../internal/render), consumed by
  [`browse.go`](../internal/webui/browse.go)), post-process the rendered tree: for
  each relative link/image, normalize it against the current dir, then map to
  `/<repo>/{raw|blob|tree}/<ref>/<path>` — `raw` for image/embed nodes, else
  `blob`/`tree` by the stat. Leave absolute/external/anchor targets alone.
- A relative link to a **missing** path is left as-is (or rendered inert) rather
  than fabricating a wrong-verb URL — a dangling link stays visibly dangling.

## Verify

- A link to a sibling `*.md` → `/<repo>/blob/<ref>/…` with the ref preserved, and
  following it renders the target (the original "empty tree" bug is gone).
- A link to a subdirectory → `/<repo>/tree/<ref>/…`; a `../` up-link resolves to
  the parent dir.
- `![](diagram.png)` → `/<repo>/raw/<ref>/…` and the image renders.
- External `https://…` links, `mailto:`, and `#anchor` are unchanged.
- A link to a nonexistent path does not 500 and is not rewritten into a false
  blob/tree URL.
- `gofmt`/`golangci-lint`/`go test ./...` clean; golden **and** failure paths
  (missing target, external-link passthrough, image→raw).
