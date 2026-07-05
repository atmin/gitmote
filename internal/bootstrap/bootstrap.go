// Package bootstrap takes an empty instance to a usable one: the first admin
// user and a token to authenticate as them — per docs/notes/bootstrap.md. The
// schema itself is created by meta.Open; this fills in the day-one rows that
// token auth otherwise makes a chicken-and-egg. Repos are created later in the
// UI, so bootstrap no longer needs one (it stays optional for scripts/tests).
package bootstrap

import (
	"context"
	"errors"
	"fmt"

	"github.com/atmin/gitmote/internal/auth"
	"github.com/atmin/gitmote/internal/meta"
)

// DefaultAdminHandle is the first admin's handle when none is given — so a plain
// `docker run` needs no extra input.
const DefaultAdminHandle = "admin"

// Options parameterize a bootstrap.
type Options struct {
	// AdminHandle is the first global admin's handle; empty means DefaultAdminHandle.
	AdminHandle string
	// RepoName is an optional initial repo, e.g. "gitmote"; empty creates none
	// (repos are made in the UI).
	RepoName string
	// DefaultBranch for the initial repo; empty means "main". Ignored without a repo.
	DefaultBranch string
	// TokenLabel labels the minted admin token; empty means "bootstrap".
	TokenLabel string
}

// Result reports what a bootstrap did. When AlreadyBootstrapped is true, the
// instance already had an admin and nothing was changed; RawToken is empty.
type Result struct {
	AlreadyBootstrapped bool
	Admin               *meta.User
	// Repo is the initial repo when one was requested, else nil.
	Repo *meta.Repo
	// RawToken is the admin's personal access token, shown exactly once. It is
	// never recoverable afterward.
	RawToken string
}

// Run bootstraps md. It refuses to clobber an existing admin: if any global
// admin already exists it returns a Result with AlreadyBootstrapped set and
// makes no changes, so re-running is safe. Otherwise it creates the admin, mints
// a token, and — only when opts.RepoName is set — the initial repo with a
// repo-admin ACL.
func Run(ctx context.Context, md *meta.Metadata, opts Options) (*Result, error) {
	handle := opts.AdminHandle
	if handle == "" {
		handle = DefaultAdminHandle
	}

	exists, err := md.AdminExists(ctx)
	if err != nil {
		return nil, err
	}
	if exists {
		return &Result{AlreadyBootstrapped: true}, nil
	}

	admin, err := md.CreateAdmin(ctx, handle)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: create admin: %w", err)
	}

	raw, err := mintToken(ctx, md, admin.ID, opts.TokenLabel)
	if err != nil {
		return nil, err
	}

	res := &Result{Admin: admin, RawToken: raw}
	if opts.RepoName != "" {
		repo, err := md.CreateRepo(ctx, opts.RepoName, opts.DefaultBranch)
		if err != nil {
			return nil, fmt.Errorf("bootstrap: create repo: %w", err)
		}
		if err := md.SetACL(ctx, repo.ID, admin.ID, meta.PermAdmin); err != nil {
			return nil, fmt.Errorf("bootstrap: grant admin acl: %w", err)
		}
		res.Repo = repo
	}
	return res, nil
}

// Reissue mints a fresh token for the existing admin (identified by handle, or
// DefaultAdminHandle) and returns it once — the recovery path for a lost token.
// Whoever can run the container against the bucket already has total infra
// control, so an out-of-band re-mint is the correct authz boundary. It requires
// an existing admin (run bootstrap first) and the writer lease (run it while the
// server is idle/stopped). No token is ever stored at rest — only its hash.
func Reissue(ctx context.Context, md *meta.Metadata, opts Options) (*Result, error) {
	handle := opts.AdminHandle
	if handle == "" {
		handle = DefaultAdminHandle
	}

	user, err := md.GetUser(ctx, handle)
	if errors.Is(err, meta.ErrNotFound) {
		return nil, fmt.Errorf("bootstrap: no user %q to reissue for; run bootstrap first", handle)
	}
	if err != nil {
		return nil, err
	}
	if !user.IsAdmin {
		return nil, fmt.Errorf("bootstrap: user %q is not an admin", handle)
	}

	label := opts.TokenLabel
	if label == "" {
		label = "bootstrap (reissued)"
	}
	raw, err := mintToken(ctx, md, user.ID, label)
	if err != nil {
		return nil, err
	}
	return &Result{Admin: user, RawToken: raw}, nil
}

// mintToken mints a personal access token for userID and stores only its hash,
// returning the raw token (shown once). label defaults to "bootstrap".
func mintToken(ctx context.Context, md *meta.Metadata, userID int64, label string) (string, error) {
	if label == "" {
		label = "bootstrap"
	}
	raw, selector, verifier, err := auth.Mint()
	if err != nil {
		return "", fmt.Errorf("bootstrap: mint token: %w", err)
	}
	if _, err := md.CreateToken(ctx, userID, selector, verifier, label); err != nil {
		return "", fmt.Errorf("bootstrap: store token: %w", err)
	}
	return raw, nil
}
