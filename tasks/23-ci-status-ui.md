# CI stage 6 — status UI + log viewer

Part of the CI epic ([16-ci.md](16-ci.md)). Depends on runs/jobs/logs existing
(stages 1–4, [17](17-ci-data-model.md)–[21](21-ci-runner.md)) and reuses the
browse UI + rendering from the shipped tasks 14/15. Admin-gated like the rest of
the UI. Additive — no write-path or protocol change.

## Spec

Surface CI in the web UI: a per-repo **runs list**, a **run detail** page (its
jobs + statuses), and a **log viewer** for a job. Add a commit-status affordance
to the existing browse commit view (a badge linking to the run for that SHA).

## Current

- `ci_runs` / `ci_jobs` with statuses (stages 1–2), and `log_key` pointing at a
  `ci/` object once a job completes (stage 4).
- The webui browse handlers ([internal/webui/browse.go](../internal/webui/browse.go))
  already split a `/browse/{repo}/-/…` path manually and gate on `requireAdmin`;
  templates + the shared `browse_head` partial are the pattern.
- [internal/render](../internal/render) sanitizes/renders HTML (markdown +
  chroma) with a size guard — the model for rendering a log safely. No ANSI→HTML
  converter yet.
- `store.Get` reads a `ci/` log blob back.

## Change

**1. Routes** (behind `requireAdmin`, same manual `/-/` split as browse):
- `GET /browse/{repo}/-/runs` — recent runs (`meta.ListRuns`), each linking to
  its detail; newest first, with a "more" marker if capped (no silent truncation).
- `GET /browse/{repo}/-/run/{id}` — run metadata + its jobs (`meta.ListJobs`) and
  statuses, each job linking to its log.
- `GET /browse/{repo}/-/run/{id}/job/{jid}/log` — the log view.

**2. Log rendering.** Fetch the blob via `store.Get(log_key)`; render as text in a
`<pre>` (reuse the blob template's isolated text block). Add a small **ANSI→HTML**
step (a minimal escape-code→`<span class>` converter, or a vetted tiny dep) so
`act`'s colored output is readable; **size-guard** it like blobs (over the cap →
plain, note shown). All output stays sanitized — log bytes are untrusted.

**3. Commit status in browse.** In `browse_commit.html` (task 14), look up the
latest run for that SHA (`meta` helper `LatestRunForSHA(repoID, sha)`) and show a
status badge linking to the run. Absent → nothing.

**4. Templates** — `ci_runs.html`, `ci_run.html`, `ci_log.html`, reusing
[layout.html](../internal/webui/templates/layout.html) and `browse_head`. A
`ci` nav affordance from the repos list (like the browse link) is fine.

## Verify

- **webui route tests** (golden + failure, still behind `requireAdmin`, cf.
  [browse_test.go](../internal/webui/browse_test.go)): seed runs/jobs + a log blob;
  the runs list renders and links; run detail shows job statuses; the log view
  renders the stored bytes; an unknown run/job → 404; unauth → redirect/deny.
- **ANSI + safety:** a log containing ANSI codes renders as spans, not raw escape
  bytes; a log with HTML-ish content is escaped (no injection); an over-cap log
  falls back to plain with the note.
- **commit badge:** a commit with a run shows the badge linking to it; one without
  shows nothing.
- `gofmt`/`golangci-lint`/`go test ./...` clean; e2e green.
