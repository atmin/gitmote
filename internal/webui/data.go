package webui

import (
	"html/template"

	"github.com/atmin/gitmote/internal/meta"
	"github.com/atmin/gitmote/internal/repo"
)

// base is embedded in the data of every page. Its fields are promoted, so every
// template references them as .Me, .IsAdmin, .Flash and .Err.
type base struct {
	Me      string // handle of the signed-in viewer for the nav (empty if anonymous)
	IsAdmin bool   // the viewer is a global admin, so show management controls
	Flash   string // success message after an action
	Err     string // error message after a failed action
	Mermaid bool   // page has a rendered mermaid diagram, so include the script
}

// loginData backs the (unauthenticated) login page.
type loginData struct {
	Err string
}

// dashboardData backs the "/" dashboard: the repo list, scoped to the viewer.
// The template gates the create form and the per-repo management links on
// .IsAdmin.
type dashboardData struct {
	base
	Repos []meta.Repo
}

// settingsData backs a repo's /settings page: its visibility and default branch.
type settingsData struct {
	base
	Repo meta.Repo
}

type usersData struct {
	base
	Users []meta.User
}

type tokensData struct {
	base
	Users    []meta.User  // for the user picker
	Selected string       // handle of the user being viewed
	Tokens   []meta.Token // the selected user's tokens
	NewToken string       // raw token, shown exactly once, right after minting
}

// accessData backs a repo's /access page: its grants. The template words them
// as spectator (read), collaborator (write) or admin.
type accessData struct {
	base
	Repo string     // the repo name
	ACLs []meta.ACL // the repo's grants
}

// secretsData backs a repo's /secrets page: the secrets it holds, by name only.
type secretsData struct {
	base
	Repo    string   // the repo name
	Names   []string // the repo's secret names (never values)
	Enabled bool     // whether a master key is configured (gates the set form)
}

// --- browse ---

// refChoice is one entry in the ref switcher. Name is the short display name,
// for example "main". Ref is the value that the switcher puts in the ref
// segment of the tree URL.
type refChoice struct {
	Name string
	Ref  string
}

// crumb is one segment of a path breadcrumb, linking to the tree at that depth.
type crumb struct {
	Name string
	Path string
}

// browseBase is embedded in every browse page. It extends the shared base for
// the nav and the flash messages. It adds these fields:
//   - the repo identity
//   - the selected ref
//   - the switcher's options
type browseBase struct {
	base
	Repo string      // repo name (a single path segment)
	Ref  string      // selected ref in the path, by default the repo's default branch
	Refs []refChoice // branches and tags for the switcher
}

type treeData struct {
	browseBase
	Path    string
	Crumbs  []crumb
	Entries []repo.TreeEntry
	Readme  template.HTML // rendered README.md for this directory, if present
}

type blobData struct {
	browseBase
	Path        string
	Crumbs      []crumb
	Text        string        // decoded text content, the plain-<pre> fallback
	Highlighted template.HTML // syntax-highlighted source, when available
	Rendered    template.HTML // rendered markdown, for a .md blob
	Binary      bool
	Size        int64
}

type refsData struct {
	browseBase
	Path   string      // always empty, present so the shared browse_head renders
	Crumbs []crumb     // always nil, present so the shared browse_head renders
	Refs   []refChoice // the repo's branches and tags
}

type commitsData struct {
	browseBase
	Path    string  // path scope, if any
	Crumbs  []crumb // breadcrumb for the scoped path (nil when unscoped)
	Commits []repo.Commit
	More    bool // history was capped: more commits exist beyond the shown page
}

type commitData struct {
	browseBase
	Path   string  // always empty, present so the shared browse_head renders
	Crumbs []crumb // always nil, present so the shared browse_head renders
	Commit repo.Commit
	Diff   template.HTML // the unified diff rendered as a colored diff (render.Diff)
	Run    *meta.Run     // the latest CI run for this commit's SHA, if any (badge)
}

type compareData struct {
	browseBase
	Path    string  // always empty, present so the shared browse_head renders
	Crumbs  []crumb // always nil, present so the shared browse_head renders
	Base    string  // base ref or SHA as given in the URL, for display and links
	Head    string  // head ref or SHA as given in the URL
	Commits []repo.Commit
	Diff    template.HTML // the three-dot diff rendered as a colored diff (render.Diff)
}

// --- ci status ---

type ciRunsData struct {
	base
	Repo string
	Runs []meta.Run
	More bool // the list was capped, and older runs exist
}

type ciRunData struct {
	base
	Repo string
	Run  meta.Run
	Jobs []meta.Job
}

type ciLogData struct {
	base
	Repo  string
	Run   meta.Run
	Job   meta.Job
	Log   template.HTML // ANSI-rendered log (safe HTML)
	Plain string        // plain-text fallback for an over-cap log
	Note  string        // an explanation when there is no rendered log
	Live  bool          // the job still runs, so the page tails the live buffer
}
