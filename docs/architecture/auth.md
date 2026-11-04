# Auth and transport

> Part of the [gitmote architecture](README.md).

## Smart HTTP comes first

gitmote serves `git http-backend` over HTTPS. Clients authenticate with bearer
personal access tokens (PATs). The `tokens` table stores them hashed. Smart HTTP
is stateless and passes through firewalls easily. So it fits a container that
scales to zero.

## Authorization is per repo

gitmote reads the authorization from `acls` on every request. There are three
levels:

| Level | Role |
| --- | --- |
| `read` | Spectator |
| `write` | Collaborator |
| `admin` | Repo management |

## Visibility gates reads, not writes

`repos.visibility` is `private` by default, or `public`. Anyone can read a public
repo with no token: clone, fetch and browse. Reading a private repo needs an ACL.
[urls.md](urls.md) describes the flat namespace that this sits in.

Writes and management are never anonymous. `git-receive-pack` always requires a
`write` or `admin` ACL. Every management action requires the global-admin role,
as the section on the management UI states.

## Only an admin can force-push the default branch

A `write` collaborator may fast-forward any branch. A collaborator may also
force-push or delete branches other than the default. A non-fast-forward push or a
deletion of `repos.default_branch` requires `admin`.

The pre-receive hook on the write path enforces this rule. The hook knows the
ancestry from the old tip to the new tip. Request authorization does not check the
rule. See [safety.md](safety.md).

## The management UI reuses the same tokens

A browser signs in by pasting a PAT once. The server verifies the PAT through the
same path as git. Then it issues a stateless session cookie, signed with HMAC.
There is no sessions table, so the UI adds no new source of truth. The management
UI needs the global-admin role (`users.is_admin`), which is distinct from per-repo
ACLs.

## SSH is deferred

Forge users expect SSH, and the `ssh_keys` schema is already in place. But an SSH
connection is stateful, which is awkward for a container that sleeps. SSH waits
until the HTTP transport is solid.
