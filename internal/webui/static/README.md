# Vendored static assets

Third-party assets served by the browse UI, embedded into the binary via
`//go:embed` (see [`../webui.go`](../webui.go)) and served same-origin from
`/ui/static/…`. Vendored — not fetched at runtime — so diagram rendering needs
no CDN, works offline, and pulls in no supply-chain dependency at request time.

## `mermaid.min.js`

Client-side [Mermaid](https://mermaid.js.org) renderer for ` ```mermaid ` blocks
in rendered markdown (the browse layout includes it only on pages that have a
diagram, and initializes it with `securityLevel: 'strict'`).

- **Version:** 11.16.0
- **Source:** `https://cdn.jsdelivr.net/npm/mermaid@11.16.0/dist/mermaid.min.js`
- **SHA-256:** `74d7c46dabca328c2294733910a8aa1ed0c37451776e8d5295da38a2b758fb9b`

### Updating

```sh
curl -fsSL "https://cdn.jsdelivr.net/npm/mermaid@<version>/dist/mermaid.min.js" \
  -o internal/webui/static/mermaid.min.js
shasum -a 256 internal/webui/static/mermaid.min.js   # record above
```

Bump the version and hash here in the same change. The single-file build exposes
`globalThis.mermaid`, so a plain `<script src>` tag is all the layout needs.
