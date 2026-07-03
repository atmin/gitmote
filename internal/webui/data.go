package webui

import (
	"html/template"

	"github.com/atmin/gitmote/internal/meta"
	"github.com/atmin/gitmote/internal/repo"
)

// base is embedded in every authenticated page's data; its fields are promoted
// so templates reference them as .Me / .Flash / .Err regardless of page.
type base struct {
	Me    string // handle of the logged-in admin, for the nav
	Flash string // success message after an action
	Err   string // error message after a failed action
}

// loginData backs the (unauthenticated) login page.
type loginData struct {
	Err string
}

type reposData struct {
	base
	Repos []meta.Repo
}

type usersData struct {
	base
	Users []meta.User
}

type tokensData struct {
	base
	Users    []meta.User  // for the user picker
	Selected string       // currently viewed user's handle
	Tokens   []meta.Token // the selected user's tokens
	NewToken string       // raw token, shown exactly once right after minting
}

type aclsData struct {
	base
	Repos    []meta.Repo // for the repo picker
	Selected string      // currently viewed repo name
	ACLs     []meta.ACL  // the selected repo's grants
}

type secretsData struct {
	base
	Repos    []meta.Repo // for the repo picker
	Selected string      // currently viewed repo name
	Names    []string    // the selected repo's secret names (never values)
	Enabled  bool        // whether a master key is configured (gates the set form)
}

// --- browse ---

// refChoice is one entry in the ref switcher: a display name (short, e.g.
// "main") and the value to pass as ?ref=.
type refChoice struct {
	Name string
	Ref  string
}

// crumb is one segment of a path breadcrumb, linking to the tree at that depth.
type crumb struct {
	Name string
	Path string
}

// browseBase is embedded in every browse page: repo identity, the selected ref,
// and the switcher's options, on top of the shared nav/flash base.
type browseBase struct {
	base
	Repo string      // full repo name (may contain slashes)
	Ref  string      // selected ref (query value), defaults to the repo's default branch
	Refs []refChoice // branches and tags for the switcher
}

type treeData struct {
	browseBase
	Path    string
	Crumbs  []crumb
	Entries []repo.TreeEntry
	Readme  template.HTML // rendered README.md for this dir, if present
}

type blobData struct {
	browseBase
	Path        string
	Crumbs      []crumb
	Text        string        // decoded text content; the plain-<pre> fallback
	Highlighted template.HTML // syntax-highlighted source, when available
	Rendered    template.HTML // rendered markdown, for a .md blob
	Binary      bool
	Size        int64
}

type commitsData struct {
	browseBase
	Path    string  // path scope, if any
	Crumbs  []crumb // breadcrumb for the scoped path (nil when unscoped)
	Commits []repo.Commit
	More    bool // history was capped; more commits exist beyond the shown page
}

type commitData struct {
	browseBase
	Path   string  // always empty; present so the shared browse_head renders
	Crumbs []crumb // always nil; present so the shared browse_head renders
	Commit repo.Commit
	Diff   string
}
