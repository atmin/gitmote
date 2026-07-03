# Syntax highlighting and markdown rendering in the browse UI

## Spec

Layer two additive niceties onto read-only repo browsing
([task 14](14-browse-repo.md)), keeping the same admin-session gate:

- **Syntax highlighting** — a text blob is shown with per-language highlighting
  instead of a bare `<pre>`. Language is chosen from the file's path/extension,
  falling back to plain text when unknown.
- **Markdown rendering** — when a directory holds a `README.md` (case-insensitive,
  `.md`/`.markdown`), render it below the tree listing as HTML. A `.md` blob view
  offers the same rendered view (raw still one click away via the existing raw
  link).

This is the Hugo stack: **goldmark** for markdown, **chroma** for code, wired
together so fenced code inside a README highlights with the same theme as blob
view.

**Out of scope** (still): blame, search, line permalinks, math/mermaid, image
rendering from the tree, diff highlighting in `browse_commit.html` (the commit
diff stays a plain `<pre>` — revisit only if the lack is felt).

## Current

Task 14 lands plain browsing: `browse_blob.html` shows a text blob as a bare
`<pre>`, and the tree page has no README. The blob text branch is deliberately a
single isolated render block (task 14 §4) — this task swaps it. No syntax/markdown
dependencies exist yet ([go.mod](../go.mod) has none).

## Change

**Dependencies** (all pure Go, no cgo — same posture as the rest of the tree):

- `github.com/alecthomas/chroma/v2` — syntax highlighter (Hugo's engine, 250+
  lexers).
- `github.com/yuin/goldmark` (+ its GFM extension) — CommonMark markdown.
- `github.com/yuin/goldmark-highlighting/v2` — routes goldmark fenced code
  through chroma, so README code blocks match blob view.
- `github.com/microcosm-cc/bluemonday` — HTML sanitizer for the rendered
  markdown output (see Safety).

**1. A render package** (e.g. `internal/render`) so webui stays thin and this
logic is unit-testable in isolation:

- `Highlight(code []byte, filename string) (template.HTML, error)` — pick a
  chroma lexer by `filename` (fallback: analyse/plain), format with the
  **class-based** HTML formatter (not inline styles), return the `<pre>`-wrapped
  markup. Emit the paired stylesheet **once** via `HighlightCSS() template.CSS`
  for the layout to include (see §3). On any lexer/format error, the caller falls
  back to the plain `<pre>` — highlighting is best-effort, never fatal.
- `Markdown(src []byte) template.HTML` — goldmark (GFM on) → **bluemonday
  UGCPolicy** sanitize → `template.HTML`. goldmark runs with **raw HTML escaped**
  (its default; do *not* set `html.WithUnsafe()`).

**2. Size guard.** Skip highlighting/markdown above a cap (e.g. 512 KiB) and fall
back to plain `<pre>` — a minified/huge blob shouldn't turn into pathological
HTML. Surface nothing special in the UI beyond the plain view; a one-line note
("shown unhighlighted — too large") is fine.

**3. Wire into webui.** Blob handler: for a text blob under the cap, call
`Highlight`; on error/oversize, keep the plain `<pre>`. Tree handler: after
listing, if a README blob exists in the directory, `Markdown` it and pass the
rendered HTML to `browse_tree.html`. Add chroma's stylesheet to
[layout.html](../internal/webui/templates/layout.html)'s `<style>` head (or a
`browse`-scoped `<style>`), from `render.HighlightCSS()`. The `.md` blob view
renders markdown in place of the highlighted source, with a toggle/link to raw.

**Safety:**

- **XSS is the headline risk** — READMEs and file contents are untrusted repo
  content, and browse is admin-gated, so a poisoned README rendered for an admin
  is a session-theft vector. Two layers: goldmark escapes raw HTML by default
  (keep it that way), **and** bluemonday UGCPolicy strips dangerous constructs
  goldmark passes through — notably `javascript:`/`data:` URLs in links. The
  sanitize step is mandatory, not optional.
- Chroma's class-based output is static markup (no scripts), safe to embed; the
  generated stylesheet is trusted (from chroma, not from repo content).

## Verify

- **Render unit tests** (`internal/render`):
  - `Highlight` picks a lexer by extension (e.g. `.go` → keywords wrapped in
    class spans), falls back to plain for an unknown extension, and returns a
    usable `<pre>` for empty input.
  - `Markdown` renders CommonMark + GFM (a table, a fenced code block that comes
    out highlighted via goldmark-highlighting).
  - **Sanitization (failure path):** a README containing `<script>…</script>`,
    an `onerror=` attribute, and a `[x](javascript:alert(1))` link renders with
    **all three neutralised** — assert the script/handler/`javascript:` scheme
    are absent from the output. This is the safety invariant; it gets an explicit
    test (CONTRIBUTING → test everything).
  - Size guard: input over the cap returns the plain-`<pre>` path.
- **webui route tests:** a `.go` blob view contains highlight class spans; a
  directory with a `README.md` shows rendered HTML on the tree page; a `.md` blob
  view renders markdown; a blob over the cap falls back to plain `<pre>`. Golden
  and failure paths, still behind `requireAdmin`.
- **Regression:** `make e2e-local` + `make e2e-restore` stay green (no write-path
  or protocol change); `gofmt`/`golangci-lint`/`go test ./...` clean.
