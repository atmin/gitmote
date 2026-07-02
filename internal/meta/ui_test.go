package meta

import (
	"context"
	"errors"
	"testing"
)

func TestSetDefaultBranch(t *testing.T) {
	ctx := context.Background()
	m := open(t)
	r := seedRepo(t, m, "atmin/app")

	if err := m.SetDefaultBranch(ctx, r.ID, "trunk"); err != nil {
		t.Fatalf("SetDefaultBranch: %v", err)
	}
	got, err := m.GetRepo(ctx, "atmin/app")
	if err != nil {
		t.Fatalf("GetRepo: %v", err)
	}
	if got.DefaultBranch != "trunk" {
		t.Errorf("default branch = %q, want trunk", got.DefaultBranch)
	}

	// Unknown repo → ErrNotFound (failure path).
	if err := m.SetDefaultBranch(ctx, 9999, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetDefaultBranch(missing) = %v, want ErrNotFound", err)
	}
}

func TestListUsersAndGetByID(t *testing.T) {
	ctx := context.Background()
	m := open(t)
	admin, err := m.CreateAdmin(ctx, "root")
	if err != nil {
		t.Fatalf("CreateAdmin: %v", err)
	}
	if _, err := m.CreateUser(ctx, "alice"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	users, err := m.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	// Ordered by handle: alice, root.
	if len(users) != 2 || users[0].Handle != "alice" || users[1].Handle != "root" {
		t.Fatalf("ListUsers = %+v, want [alice root]", users)
	}
	if users[0].IsAdmin || !users[1].IsAdmin {
		t.Errorf("is_admin flags wrong: %+v", users)
	}

	got, err := m.GetUserByID(ctx, admin.ID)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if got.Handle != "root" || !got.IsAdmin {
		t.Errorf("GetUserByID = %+v, want root/admin", got)
	}
	if _, err := m.GetUserByID(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetUserByID(missing) = %v, want ErrNotFound", err)
	}
}

func TestDeleteToken(t *testing.T) {
	ctx := context.Background()
	m := open(t)
	u, err := m.CreateUser(ctx, "alice")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	tok, err := m.CreateToken(ctx, u.ID, "sel", "ver", "laptop")
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if err := m.DeleteToken(ctx, tok.ID); err != nil {
		t.Fatalf("DeleteToken: %v", err)
	}
	toks, err := m.ListTokens(ctx, u.ID)
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	if len(toks) != 0 {
		t.Errorf("after delete ListTokens = %+v, want empty", toks)
	}
	// Idempotent: deleting again is not an error.
	if err := m.DeleteToken(ctx, tok.ID); err != nil {
		t.Errorf("second DeleteToken = %v, want nil", err)
	}
}

func TestListAndDeleteACL(t *testing.T) {
	ctx := context.Background()
	m := open(t)
	r := seedRepo(t, m, "atmin/app")
	alice, _ := m.CreateUser(ctx, "alice")
	bob, _ := m.CreateUser(ctx, "bob")
	if err := m.SetACL(ctx, r.ID, bob.ID, PermWrite); err != nil {
		t.Fatalf("SetACL bob: %v", err)
	}
	if err := m.SetACL(ctx, r.ID, alice.ID, PermRead); err != nil {
		t.Fatalf("SetACL alice: %v", err)
	}

	acls, err := m.ListACLs(ctx, r.ID)
	if err != nil {
		t.Fatalf("ListACLs: %v", err)
	}
	// Ordered by handle: alice(read), bob(write).
	if len(acls) != 2 || acls[0].Handle != "alice" || acls[0].Perm != PermRead ||
		acls[1].Handle != "bob" || acls[1].Perm != PermWrite {
		t.Fatalf("ListACLs = %+v", acls)
	}

	if err := m.DeleteACL(ctx, r.ID, bob.ID); err != nil {
		t.Fatalf("DeleteACL: %v", err)
	}
	acls, _ = m.ListACLs(ctx, r.ID)
	if len(acls) != 1 || acls[0].Handle != "alice" {
		t.Errorf("after delete ListACLs = %+v, want [alice]", acls)
	}
	// Revoked user now has no access.
	if _, err := m.GetACL(ctx, r.ID, bob.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetACL after revoke = %v, want ErrNotFound", err)
	}
}
