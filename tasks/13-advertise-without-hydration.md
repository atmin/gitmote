# Serve the ref advertisement without hydrating objects

## Spec

The `info/refs` advertisement — the GET that opens every clone, fetch, and
`ls-remote` — must not hydrate object history. It only lists refs, which gitmote
already holds in s3lite. Today a cold advertisement downloads the whole repo just
to print ref names; it shouldn't touch a single object.

This is the first, separable slice of bounded hydration
([docs/notes/object-hydration.md](../docs/notes/object-hydration.md)); the full
per-operation closure for the data POSTs stays deferred (it needs a reachability
index — see the note).

## Current

[materialize.go](../internal/repo/materialize.go) `Materialize` runs on **every**
request — including the `info/refs` GET — and always `hydrateObjects` (full
closure) then `writeRefs` via `git update-ref`, which *requires each ref's target
object present*. So the advertisement path drags the entire repo onto disk before
git prints a ref list.

## Why it's safe (spiked)

`git upload-pack --advertise-refs` advertises refs whose target objects are
**absent** — verified against git 2.50:

- Refs written into `packed-refs` with no objects present → full, correct
  advertisement (SHAs, `symref=HEAD`, capabilities, exit 0).
- An annotated-tag ref with its tag object absent → advertised fine; git just
  **omits the `^{}` peel line** (correct, only a lost negotiation hint). With a
  pre-computed `^<commit>` peel line in `packed-refs`, it emits the peel too —
  still no object needed.

The advertisement and the later data POST are both served by the *same* stock
git, so there is no capability/protocol drift risk — we only skip hydration.

## Change

- Add a refs-only path to the Materializer (e.g. `MaterializeRefs(ctx, name)`):
  ensure the bare repo exists, regenerate `packed-refs` **directly from
  `meta.ListRefs`** (write the file — not `update-ref`, which validates object
  presence), set `HEAD` via `symbolic-ref` from `default_branch`. **No object
  hydration.** MVP omits annotated-tag `^{}` peels (advertisement stays correct);
  storing peels in meta for the optimization is an optional follow-up.
- In [backend.go](../internal/githttp/backend.go) `ServeHTTP`, when
  `endpoint == "info/refs"` (either service), call `MaterializeRefs`. The data
  POSTs (`git-upload-pack`, `git-receive-pack`) keep the full `Materialize` —
  bounding *those* closures is the deferred task.
- Factor the `packed-refs` generation so the refs-only and full paths agree on
  ref formatting.

**Notes / out of scope:** refs-only `packed-refs` and a later full-`Materialize`'s
loose refs coexist (loose overrides — fine). Pruning refs deleted from meta is a
pre-existing cache gap, unchanged here. The data-transfer POSTs still full-hydrate
until the bounded-closure task lands.

## Verify

- **No objects touched:** an `info/refs` advertisement (both services) served from
  a repo whose objects were never hydrated returns the correct ref list — assert
  via a spy `store` that `Get` is **never** called on the advertisement path, and
  the advertised refs/SHAs match `meta.ListRefs`. Cover a repo with an annotated
  tag (advertises, peel omitted).
- **Data path intact:** a real clone/fetch and a push still work end-to-end (the
  POSTs full-hydrate as before) — existing githttp read/write tests stay green.
- **Regression:** `make e2e-local` + `make e2e-restore` green; `go test ./...`.
- Update materialize.go's "full-hydrate for the MVP" comment to reflect that the
  advertisement is now objectless.
