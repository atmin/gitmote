package webui

import "github.com/atmin/gitmote/internal/meta"

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
