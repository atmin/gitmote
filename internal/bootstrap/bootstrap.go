// Package bootstrap takes an empty instance to a usable one: the first admin
// user, a token to authenticate as them, and an initial repo — per
// docs/notes/bootstrap.md. The schema itself is created by meta.Open; this fills
// in the day-one rows that token auth otherwise makes a chicken-and-egg.
package bootstrap

import (
	"context"
	"fmt"

	"github.com/atmin/gitmote/internal/auth"
	"github.com/atmin/gitmote/internal/meta"
)

// Options parameterize a bootstrap.
type Options struct {
	// AdminHandle is the first global admin's handle. Required.
	AdminHandle string
	// RepoName is the initial repo, e.g. "atmin/gitmote". Required.
	RepoName string
	// DefaultBranch for the initial repo; empty means "main".
	DefaultBranch string
	// TokenLabel labels the minted admin token; empty means "bootstrap".
	TokenLabel string
}

// Result reports what a bootstrap did. When AlreadyBootstrapped is true, the
// instance already had an admin and nothing was changed; RawToken is empty.
type Result struct {
	AlreadyBootstrapped bool
	Admin               *meta.User
	Repo                *meta.Repo
	// RawToken is the admin's personal access token, shown exactly once. It is
	// never recoverable afterward.
	RawToken string
}

// Run bootstraps md. It refuses to clobber an existing admin: if any global
// admin already exists it returns a Result with AlreadyBootstrapped set and
// makes no changes, so re-running is safe. Otherwise it creates the admin,
// mints a token, creates the initial repo, and grants the admin repo-admin
// access.
func Run(ctx context.Context, md *meta.Metadata, opts Options) (*Result, error) {
	if opts.AdminHandle == "" {
		return nil, fmt.Errorf("bootstrap: admin handle is required")
	}
	if opts.RepoName == "" {
		return nil, fmt.Errorf("bootstrap: repo name is required")
	}

	exists, err := md.AdminExists(ctx)
	if err != nil {
		return nil, err
	}
	if exists {
		return &Result{AlreadyBootstrapped: true}, nil
	}

	admin, err := md.CreateAdmin(ctx, opts.AdminHandle)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: create admin: %w", err)
	}

	raw, selector, verifier, err := auth.Mint()
	if err != nil {
		return nil, fmt.Errorf("bootstrap: mint token: %w", err)
	}
	label := opts.TokenLabel
	if label == "" {
		label = "bootstrap"
	}
	if _, err := md.CreateToken(ctx, admin.ID, selector, verifier, label); err != nil {
		return nil, fmt.Errorf("bootstrap: store token: %w", err)
	}

	repo, err := md.CreateRepo(ctx, opts.RepoName, opts.DefaultBranch)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: create repo: %w", err)
	}
	if err := md.SetACL(ctx, repo.ID, admin.ID, meta.PermAdmin); err != nil {
		return nil, fmt.Errorf("bootstrap: grant admin acl: %w", err)
	}

	return &Result{Admin: admin, Repo: repo, RawToken: raw}, nil
}
