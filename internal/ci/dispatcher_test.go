package ci

import (
	"context"
	"errors"
	"testing"

	"github.com/atmin/gitmote/internal/meta"
)

// fakeRuns records CreateRun calls, optionally returning err.
type fakeRuns struct {
	created []meta.Run
	err     error
}

func (f *fakeRuns) CreateRun(_ context.Context, repoID int64, ref, sha string) (*meta.Run, error) {
	if f.err != nil {
		return nil, f.err
	}
	run := meta.Run{ID: int64(len(f.created) + 1), RepoID: repoID, Ref: ref, SHA: sha, Status: meta.RunQueued}
	f.created = append(f.created, run)
	return &run, nil
}

const (
	shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestDispatchBranchUpdateCreatesRun(t *testing.T) {
	f := &fakeRuns{}
	d := NewDispatcher(f, nil)
	d.Dispatch(context.Background(), Event{
		RepoID: 7, RepoName: "atmin/repo", Ref: "refs/heads/main", OldSHA: shaA, NewSHA: shaB,
	})
	if len(f.created) != 1 {
		t.Fatalf("created %d runs, want 1", len(f.created))
	}
	got := f.created[0]
	if got.RepoID != 7 || got.Ref != "refs/heads/main" || got.SHA != shaB {
		t.Errorf("run = %+v, want repo 7 ref refs/heads/main sha %q", got, shaB)
	}
}

func TestDispatchIgnoresTagsAndDeletes(t *testing.T) {
	cases := []struct {
		name string
		ev   Event
	}{
		{"tag push", Event{RepoID: 1, Ref: "refs/tags/v1", OldSHA: "", NewSHA: shaA}},
		{"branch delete (zero sha)", Event{RepoID: 1, Ref: "refs/heads/main", OldSHA: shaA, NewSHA: meta.ZeroSHA}},
		{"branch delete (empty sha)", Event{RepoID: 1, Ref: "refs/heads/main", OldSHA: shaA, NewSHA: ""}},
		{"non-branch ref", Event{RepoID: 1, Ref: "refs/notes/commits", OldSHA: "", NewSHA: shaA}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeRuns{}
			NewDispatcher(f, nil).Dispatch(context.Background(), tc.ev)
			if len(f.created) != 0 {
				t.Errorf("created %d runs, want 0", len(f.created))
			}
		})
	}
}

func TestDispatchBranchCreateCreatesRun(t *testing.T) {
	f := &fakeRuns{}
	// A new branch: old is zero, new is a real sha — one run.
	NewDispatcher(f, nil).Dispatch(context.Background(), Event{
		RepoID: 1, Ref: "refs/heads/feature", OldSHA: meta.ZeroSHA, NewSHA: shaA,
	})
	if len(f.created) != 1 {
		t.Fatalf("created %d runs, want 1", len(f.created))
	}
}

func TestDispatchSwallowsCreateError(t *testing.T) {
	// A failed enqueue must not panic or propagate — the push already committed.
	f := &fakeRuns{err: errors.New("db down")}
	NewDispatcher(f, nil).Dispatch(context.Background(), Event{
		RepoID: 1, Ref: "refs/heads/main", OldSHA: shaA, NewSHA: shaB,
	})
	if len(f.created) != 0 {
		t.Errorf("created %d runs, want 0", len(f.created))
	}
}
