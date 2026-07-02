# 09 — Management web UI

Depends on: 06, 08.

## Spec

A minimal authenticated web UI for the operations you can't do over git:
create/list repos, mint/revoke tokens, and manage per-repo ACLs. Backed by the
same s3lite tables (the "web UI" component in
[storage.md](../docs/architecture/storage.md)). Repo browsing and web-authored
commits stay out of scope.

## Current

Management is CLI-only (bootstrap) plus direct git. No UI.

## Change

- Server-rendered pages (Go `html/template`) under an authed prefix — **decide:**
  browser login as a session cookie vs. a token pasted into the UI.
- Screens: repos (list / create / set default branch), tokens (mint shows the
  raw token once, revoke), ACLs (grant/revoke read/write/admin per user).
- Reuse the task-03 query layer and task-06 auth; introduce no new source of
  truth.

## Verify

- Handler tests: golden path for create-repo / mint-token / grant-ACL, **plus**
  auth rejections (unauthenticated and non-admin → denied).
- Manual: create a repo in the UI, mint a token, push to it.
- Non-breaking: additive routes behind auth; git paths untouched.
