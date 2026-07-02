package meta

import (
	"context"
	"errors"
	"testing"
)

const (
	shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	shaC = "cccccccccccccccccccccccccccccccccccccccc"
)

// refSHA returns the current sha of a ref, or "" if absent.
func refSHA(t *testing.T, m *Metadata, repoID int64, name string) string {
	t.Helper()
	refs, err := m.ListRefs(context.Background(), repoID)
	if err != nil {
		t.Fatalf("ListRefs: %v", err)
	}
	for _, r := range refs {
		if r.Name == name {
			return r.SHA
		}
	}
	return ""
}

func TestCASRefCreateUpdateDelete(t *testing.T) {
	ctx := context.Background()
	m := open(t)
	r := seedRepo(t, m, "atmin/repo")
	const main = "refs/heads/main"

	// Create: old is the zero id (ref must be absent).
	if err := m.CASRef(ctx, r.ID, main, ZeroSHA, shaA); err != nil {
		t.Fatalf("CASRef create: %v", err)
	}
	if got := refSHA(t, m, r.ID, main); got != shaA {
		t.Fatalf("after create sha = %q, want %q", got, shaA)
	}

	// Update: old must match the current value.
	if err := m.CASRef(ctx, r.ID, main, shaA, shaB); err != nil {
		t.Fatalf("CASRef update: %v", err)
	}
	if got := refSHA(t, m, r.ID, main); got != shaB {
		t.Fatalf("after update sha = %q, want %q", got, shaB)
	}

	// Delete: new is the zero id.
	if err := m.CASRef(ctx, r.ID, main, shaB, ZeroSHA); err != nil {
		t.Fatalf("CASRef delete: %v", err)
	}
	if got := refSHA(t, m, r.ID, main); got != "" {
		t.Fatalf("after delete sha = %q, want absent", got)
	}
}

func TestCASRefRejectsAndRollsBack(t *testing.T) {
	ctx := context.Background()
	m := open(t)
	r := seedRepo(t, m, "atmin/repo")
	const main = "refs/heads/main"

	if err := m.CASRef(ctx, r.ID, main, ZeroSHA, shaA); err != nil {
		t.Fatalf("seed create: %v", err)
	}

	// Stale expected value: a non-fast-forward the server must refuse.
	var mm *CASMismatchError
	err := m.CASRef(ctx, r.ID, main, shaB, shaC)
	if !errors.As(err, &mm) {
		t.Fatalf("CASRef mismatch = %v, want *CASMismatchError", err)
	}
	if mm.Got != shaA || mm.Want != shaB {
		t.Errorf("mismatch = %+v, want Want=%s Got=%s", mm, shaB, shaA)
	}
	// The ref is unchanged — the rejected update rolled back.
	if got := refSHA(t, m, r.ID, main); got != shaA {
		t.Errorf("after rejected update sha = %q, want %q (unchanged)", got, shaA)
	}

	// Create against an already-present ref is also a mismatch.
	if err := m.CASRef(ctx, r.ID, main, ZeroSHA, shaB); !errors.As(err, &mm) {
		t.Errorf("CASRef create-over-existing = %v, want *CASMismatchError", err)
	}
}

func TestCASRefsAtomicMultiRef(t *testing.T) {
	ctx := context.Background()
	m := open(t)
	r := seedRepo(t, m, "atmin/repo")
	const (
		main = "refs/heads/main"
		dev  = "refs/heads/dev"
	)

	// One bad update in the batch rolls back the whole thing — the good
	// create for main must not survive.
	err := m.CASRefs(ctx, r.ID, []RefUpdate{
		{Name: main, Old: ZeroSHA, New: shaA},
		{Name: dev, Old: shaB, New: shaC}, // wrong: dev is absent, not shaB
	})
	var mm *CASMismatchError
	if !errors.As(err, &mm) || mm.Name != dev {
		t.Fatalf("CASRefs = %v, want *CASMismatchError for %q", err, dev)
	}
	if got := refSHA(t, m, r.ID, main); got != "" {
		t.Errorf("main sha = %q after failed batch, want absent (rolled back)", got)
	}

	// An all-valid batch commits both.
	if err := m.CASRefs(ctx, r.ID, []RefUpdate{
		{Name: main, Old: ZeroSHA, New: shaA},
		{Name: dev, Old: ZeroSHA, New: shaB},
	}); err != nil {
		t.Fatalf("CASRefs valid batch: %v", err)
	}
	if got := refSHA(t, m, r.ID, main); got != shaA {
		t.Errorf("main sha = %q, want %q", got, shaA)
	}
	if got := refSHA(t, m, r.ID, dev); got != shaB {
		t.Errorf("dev sha = %q, want %q", got, shaB)
	}
}
