package meta

import (
	"context"
	"errors"
	"testing"
)

func TestCreateRun(t *testing.T) {
	ctx := context.Background()
	m := open(t)
	r := seedRepo(t, m, "repo")
	const main = "refs/heads/main"

	run, err := m.CreateRun(ctx, r.ID, main, shaA)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if run.RepoID != r.ID || run.Ref != main || run.SHA != shaA {
		t.Errorf("run = %+v, want repo %d ref %q sha %q", run, r.ID, main, shaA)
	}
	if run.Status != RunQueued {
		t.Errorf("status = %q, want %q", run.Status, RunQueued)
	}

	// Round-trips through GetRun.
	got, err := m.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.ID != run.ID || got.Status != RunQueued || got.SHA != shaA {
		t.Errorf("GetRun = %+v, want id %d queued sha %q", got, run.ID, shaA)
	}
}

func TestSetRunStatus(t *testing.T) {
	ctx := context.Background()
	m := open(t)
	r := seedRepo(t, m, "repo")

	run, err := m.CreateRun(ctx, r.ID, "refs/heads/main", shaA)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := m.SetRunStatus(ctx, run.ID, RunRunning); err != nil {
		t.Fatalf("SetRunStatus: %v", err)
	}
	got, _ := m.GetRun(ctx, run.ID)
	if got.Status != RunRunning {
		t.Errorf("status = %q, want %q", got.Status, RunRunning)
	}

	// An unknown run is ErrNotFound.
	if err := m.SetRunStatus(ctx, 9999, RunPassed); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetRunStatus(missing) = %v, want ErrNotFound", err)
	}
	// GetRun on an unknown id is ErrNotFound.
	if _, err := m.GetRun(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetRun(missing) = %v, want ErrNotFound", err)
	}
}

func TestListRunsNewestFirst(t *testing.T) {
	ctx := context.Background()
	m := open(t)
	r := seedRepo(t, m, "repo")
	other := seedRepo(t, m, "other")

	first, _ := m.CreateRun(ctx, r.ID, "refs/heads/main", shaA)
	second, _ := m.CreateRun(ctx, r.ID, "refs/heads/main", shaB)
	// A run for a different repo must not appear in r's list.
	if _, err := m.CreateRun(ctx, other.ID, "refs/heads/main", shaC); err != nil {
		t.Fatalf("CreateRun(other): %v", err)
	}

	runs, err := m.ListRuns(ctx, r.ID, 0)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("ListRuns = %d runs, want 2", len(runs))
	}
	if runs[0].ID != second.ID || runs[1].ID != first.ID {
		t.Errorf("ListRuns order = [%d %d], want newest-first [%d %d]",
			runs[0].ID, runs[1].ID, second.ID, first.ID)
	}

	// limit caps the result.
	limited, err := m.ListRuns(ctx, r.ID, 1)
	if err != nil {
		t.Fatalf("ListRuns(limit): %v", err)
	}
	if len(limited) != 1 || limited[0].ID != second.ID {
		t.Errorf("ListRuns(limit 1) = %+v, want just newest %d", limited, second.ID)
	}
}
