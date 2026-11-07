# Vendored static assets

This directory holds third-party assets for the browse UI. `//go:embed` embeds
them into the binary (see [`../webui.go`](../webui.go)). The server serves them
from its own origin, under `/ui/static/`. The repo includes these assets, and
the server never fetches them at runtime. So diagram rendering:

- needs no CDN
- works offline
- adds no supply-chain dependency at request time

## `mermaid.min.js`

This file is the [Mermaid](https://mermaid.js.org) renderer for ` ```mermaid `
blocks in rendered Markdown. It runs in the browser. The browse layout includes
it only on pages that have a diagram. It initializes Mermaid with
`securityLevel: 'strict'`.

- **Version:** 11.16.0
- **Source:** `https://cdn.jsdelivr.net/npm/mermaid@11.16.0/dist/mermaid.min.js`
- **SHA-256:** `74d7c46dabca328c2294733910a8aa1ed0c37451776e8d5295da38a2b758fb9b`

### Updating

```sh
curl -fsSL "https://cdn.jsdelivr.net/npm/mermaid@<version>/dist/mermaid.min.js" \
  -o internal/webui/static/mermaid.min.js
shasum -a 256 internal/webui/static/mermaid.min.js   # record the hash above
```

Update the version and the hash in this file in the same change. The single-file
build exposes `globalThis.mermaid`, so the layout needs only a plain
`<script src>` tag.
