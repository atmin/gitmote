package ci

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

func TestNoopTrigger(t *testing.T) {
	if err := (NoopTrigger{}).Trigger(context.Background(), map[string]string{"X": "1"}); err != nil {
		t.Fatalf("NoopTrigger.Trigger = %v, want nil", err)
	}
}

func TestFlattenEnv(t *testing.T) {
	got := flattenEnv(map[string]string{"A": "1", "B": "2"})
	sort.Strings(got)
	if len(got) != 2 || got[0] != "A=1" || got[1] != "B=2" {
		t.Errorf("flattenEnv = %v, want [A=1 B=2]", got)
	}
}

// TestLocalTriggerSpawnsRunnerWithEnv proves the local trigger runs the runner
// binary with the injected env — the local analog of Scaleway starting a job.
func TestLocalTriggerSpawnsRunnerWithEnv(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	script := filepath.Join(dir, "fake-runner")
	// A stand-in runner: record the injected coordinates so the test can assert
	// they were passed through.
	body := "#!/bin/sh\nprintf '%s %s' \"$GITMOTE_CI_JOB_ID\" \"$GITMOTE_CI_CLONE_TOKEN\" > \"$OUT_FILE\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	tr := NewLocalTrigger(script, nil)
	if err := tr.Trigger(context.Background(), map[string]string{
		"GITMOTE_CI_JOB_ID":      "7",
		"GITMOTE_CI_CLONE_TOKEN": "tok",
		"OUT_FILE":               out,
	}); err != nil {
		t.Fatalf("Trigger: %v", err)
	}

	// Fire-and-forget: poll for the spawned runner to finish and write the file.
	var data []byte
	for range 200 {
		if b, err := os.ReadFile(out); err == nil {
			data = b
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if string(data) != "7 tok" {
		t.Errorf("spawned runner recorded %q, want %q", data, "7 tok")
	}
}

func TestLocalTriggerMissingBinaryErrors(t *testing.T) {
	tr := NewLocalTrigger(filepath.Join(t.TempDir(), "nope"), nil)
	if err := tr.Trigger(context.Background(), nil); err == nil {
		t.Error("Trigger with a missing binary = nil, want an error")
	}
}
