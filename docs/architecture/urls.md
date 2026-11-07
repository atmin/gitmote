# URLs

gitmote is a personal instance. It has one owner, a few repos and a few invited
people. This one fact sets the shape of every URL.

Repos live in a flat namespace, where each repo is one path segment, as in
`git.example.com/<repo>`. There is no `user/repo` form. gitmote has no per-user
namespace, so there is nothing to tell apart.

Read [auth.md](auth.md) for the access model. Read [storage.md](storage.md) for
the tables that hold this state, such as `repos` and `acls`.

## The namespace

A repo is one path segment. Every URL that is not repo content is a verb under
the repo, or a top-level global route.

| What | URL | Access |
| --- | --- | --- |
| Dashboard: the list of repos | `/` | per viewer |
| Repo landing: the README and the tree at the default branch | `/<repo>`, plus git's own suffixes | read |
| Clone or fetch | `GET /<repo>/info/refs?service=git-upload-pack`, `POST /<repo>/git-upload-pack` | read |
| Push | `POST /<repo>/git-receive-pack` | write |
| Browse a directory or a file | `/<repo>/tree/<ref>/<path…>` | read |
| A file, explicitly | `/<repo>/blob/<ref>/<path…>` | read |
| Raw bytes | `/<repo>/raw/<ref>/<path…>` | read |
| History, or one commit | `/<repo>/commits/<ref>/<path…>`, `/<repo>/commit/<sha>` | read |
| Compare two refs | `/<repo>/compare/<base>...<head>` | read |
| Branches and tags | `/<repo>/refs` | read |
| CI runs, or one run | `/<repo>/runs`, `/<repo>/runs/<id>` | read |
| Settings: default branch, visibility, rename, delete | `/<repo>/settings` | admin |
| Access: invite or revoke collaborators and spectators | `/<repo>/access` | admin |
| CI secrets | `/<repo>/secrets` | admin |
| Login and logout | `/login`, `/logout` | anyone |
| Users | `/users` | admin |
| Personal access tokens | `/tokens` | admin |
| Static assets | `/static/…` | anyone |
| Liveness and version | `/healthz`, `/version` | anyone |

### Repo names

A repo name follows these rules:

- It is one path segment, with no `/`, and it is not empty.
- It does not start with `.`, and it is not the bare `-`.
- It is not a reserved name.
- gitmote strips a trailing `.git`. So `clone host/gitmote.git` reaches the same
  repo as `host/gitmote`.

gitmote checks a name against these rules when it creates the repo. It rejects a
bad name with a clear error.

The reserved names are a small fixed set: the top-level global routes. Some are
live routes, and the others are reserved ahead of need.

| Reserved names | Status |
| --- | --- |
| `login`, `logout`, `users`, `tokens` | Live: the global routes of the UI |
| `static`, `healthz`, `version` | Live: static assets, liveness and version |
| `internal` | Live: the CI report API ([ci.md](ci.md)) |
| `settings`, `new`, `search`, `api`, `metrics` | Reserved ahead of need |

Once a repo owns a name, freeing that name is a breaking rename. Reserving the
name early costs the single owner nothing.

No other name is reserved. The verbs for content and actions, such as `tree`,
`blob`, `raw`, `commits` and `runs`, sit at segment 2. So a repo named `tree` is
legal. `/tree/tree/main/x` means repo `tree`, verb `tree`, ref `main` and path
`x`.

Git's own suffixes at segment 2 are `info`, `git-upload-pack`,
`git-receive-pack`, `HEAD` and `objects`. They and the UI verbs are two fixed
sets with no word in common.

## Content addressing

Three verbs address repo content. The ref is always in the path, never in a
query parameter.

| Path | What it serves |
| --- | --- |
| `tree/<ref>/<path>` | The canonical, unified view. A directory renders a listing. A file renders its view: markdown or highlighted source. The server detects which from the tree. |
| `blob/<ref>/<path>` | An explicit file view. When the path is a directory, it returns a 301 to `tree/<ref>/<path>`, not a 404. |
| `raw/<ref>/<path>` | The bytes of a file. A directory returns a 404. |

So a `blob` URL for a directory still works, and `tree` never guesses wrong.

The UI and the markdown rewriter emit `blob` for a file and `tree` for a
directory. Such a URL describes its target, in the form that GitHub users know.
Both verbs stay tolerant as above. So a link with the wrong verb still resolves.
This holds for a link written by hand, or from outside.

### Ref resolution

Both `<ref>` and `<path>` may contain slashes. So the server resolves the ref
greedily. It takes the longest leading prefix of the path that names a real ref.
A real ref is a branch or a tag of the repo, as the metadata database lists it.

For example, take `/<repo>/tree/feature/login/src/x.go` in a repo with a branch
`feature/login`. The ref is `feature/login`, and the path is `src/x.go`.

A URL with no ref, such as the bare `/<repo>` landing, means the default branch of
the repo.

## Relative links in rendered content

The ref in the path makes relative links work. Take a link inside
`/<repo>/tree/main/docs/readme.md`. It resolves against that URL, so it keeps
`<repo>` and `<ref>` with no extra work.

A bare relative link cannot carry two things, so the renderer supplies them. It
rewrites the relative links in rendered markdown:

- An embedded asset, such as `![](diagram.png)`, must point at the `raw` bytes,
  not at an HTML view. The browser cannot infer this. So the renderer rewrites
  images and embeds to `/<repo>/raw/<ref>/…`.
- A navigation link gets the precise verb and the current ref. The verb is
  `blob` for a file and `tree` for a directory, and a tree lookup decides which.

The renderer leaves three kinds of link untouched:

- absolute URLs
- external `http(s)` links
- in-page anchors (`#…`)

## Access and visibility

Users exist so that the owner can invite people. Access is per repo. gitmote
reads it from the `acls` table on every request ([auth.md](auth.md)).

| ACL level | Role |
| --- | --- |
| `read` | spectator |
| `write` | collaborator |
| `admin` | repo management |

To invite someone, the owner creates or selects a user and grants an ACL. Invites
add no new permission model.

### Visibility

`repos.visibility` is `private`, the default, or `public`. Anyone can read a
public repo with no token, to clone, fetch and browse it.

Writes are never anonymous. `git-receive-pack` needs a `write` or `admin` ACL,
on a public repo too. Management in the UI needs the global admin role
([auth.md](auth.md)).

The repo-read check gates every read, both browse and fetch. Anyone may read a
public repo. A private repo needs a `read` ACL. Global admin is not the gate.
This is what makes spectators and public repos usable.

### Force-push to the default branch

Only `admin` may force-push the default branch. A `write` collaborator may
fast-forward any branch, the default branch included. A collaborator may also
force-push or delete any branch other than the default. A non-fast-forward or a
deletion of the default branch requires `admin`.

This protects the history of the branch that everyone tracks from a rewrite by a
collaborator. The rule does not affect the everyday push, a fast-forward.

The write path enforces this rule. There, gitmote knows how the old SHA relates
to the new SHA. The check runs at the ref compare-and-swap (CAS), in the
pre-receive hook ([safety.md](safety.md)). It does not run when gitmote first
authorizes the request.

### No per-repo owner

Ownership of a repo is not part of the namespace. Repos belong to the instance,
and the admin owns them all implicitly. ACLs grant access to everyone else. The
flat namespace makes this simplification possible.

## Constraints on the implementation

The flat namespace creates two constraints that are easy to miss.

### The UI routes share the mux with git

The browse routes and the per-repo management routes share one mux with the git
smart-HTTP catch-all. The UI's `/{repo}/…` routes sit beside the git endpoints:

- `info/refs`
- `git-upload-pack` and `git-receive-pack`
- `HEAD` and `objects/`

The git handler owns the `/` catch-all ([request-flows.md](request-flows.md)).

One rule keeps clone and push working. Register only enumerated verb routes, such
as these:

- `GET /{repo}` and `GET /{repo}/tree/…`
- `/{repo}/blob/…`
- `/{repo}/settings`, `/{repo}/access` and `/{repo}/secrets`

Never register a broad `/{repo}/{rest...}`, because it would catch the git
endpoints. Go's ServeMux picks the most specific pattern. So it routes the verbs
to the UI, and everything else on `/{repo}/…` to the git catch-all. The landing
`GET /{repo}` has one segment, so it differs from `GET /{repo}/info/refs`.

The top-level global routes are `/`, `/login`, `/users`, `/tokens` and
`/static/…`. Each one is a literal or an exact-root pattern. So each one wins
over `GET /{repo}` without a broad match. The reserved names keep a repo from
ever claiming one of these routes.

### The force-push guard lives in the pre-receive hook

The guard for the default branch lives in the pre-receive hook. It must reject
each ref on its own. Then the client sees a clean refusal, not a failure after the
fact. The refusal reads
`force-push or deletion of the default branch "main" requires admin`.

The guard needs three inputs at that point:

1. The pusher's ACL level, `admin` or `write`. Elsewhere, the write path only
   checks for `write` or higher.
2. The repo's `default_branch`.
3. For each ref, whether the update is a non-fast-forward or a deletion. In a
   non-fast-forward, the old SHA is not an ancestor of the new SHA. A deletion
   has a new SHA of all zeros.

## Why the URLs look like this

### No `/-/` separator

GitLab needs `/-/`, because its groups can nest to any depth, as in
`group/sub/…/project`. So the boundary between the project and the verb is
ambiguous there. GitHub has a fixed `owner/repo`, so it needs no marker.

gitmote is simpler still. The repo is one segment, so the verb is always
segment 2. A marker would add nothing, so no gitmote URL uses one.

### Reserved names are a multi-tenant problem

A list of reserved names is a problem for GitHub, because users pick names that
GitHub does not control. That is why GitHub keeps a list of reserved usernames.

On a personal instance, the owner names every repo. So a small fixed set of
reserved names, the global routes above, causes no problem. The owner simply
never names a repo `login`.

### The `.` and `-` rules keep room for the future

The rules on `.` and `-` are not features. They keep room for routes that nobody
has planned yet. Reserving names that start with `.`, and the bare `-`, costs the
owner nothing.

- A future well-known route can live under `.well-known/`.
- A future system namespace can use `-/…`.

Neither one forces a breaking rename of an existing repo. So the list of reserved
words only needs the names that should be short and top-level. Every other route
can live under `.` or `-`. Reserving a name ahead of need costs little. Freeing a
name after a repo claims it costs a breaking rename.

### Greedy refs are safe

Greedy resolution might look ambiguous: is `a/b` the ref `a` or the ref `a/b`?
This mostly cannot happen. Git's ref store forbids a branch `a` and a branch `a/b`
at the same time. A name cannot be both a file and a directory under
`refs/heads`.

The only case left is a branch and a tag with the same prefix. A fixed precedence
resolves it: the branch wins.

A ref in the path keeps the URL readable. It needs no `%2F` encoding, which causes
problems with proxies and `net/http`.

Compare carries two refs in one segment: `compare/<base>...<head>`. Either ref may
contain slashes. The server splits the segment on the literal `...`. Git's ref
store forbids `..` in a ref name. So the separator is unambiguous, even when
either ref contains slashes.

### One verb for content, and a tolerant `blob`

With one content verb, a relative link resolves correctly for a file and for a
directory. The author of the link does not need to know which. Because `blob`
and `tree` are tolerant, an imprecise link still reaches a valid page instead of a
404.

### The ref in the path, not in the query

A relative link drops a ref that sits in the query, such as `?ref=master`. So an
in-repo markdown link loses the ref and shows an empty tree. A ref in the path
survives relative resolution, and a user can share the URL.
