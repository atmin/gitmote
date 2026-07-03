package ci

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
)

// NoopTrigger records a run but executes nothing. It is the fallback when no
// runner substrate is configured (neither local nor Scaleway) — dispatch stays
// fire-and-forget and the run simply never leaves the queued state.
type NoopTrigger struct{}

// Trigger implements Trigger.
func (NoopTrigger) Trigger(context.Context, map[string]string) error { return nil }

// LocalTrigger runs a CI job by spawning the gitmote-runner binary as a local
// process, mirroring how the Scaleway client starts a container: same runner
// code, same env contract, a local substrate instead of a cloud job. It is
// fire-and-forget — Start (not Wait) returns immediately so dispatch never
// blocks the push; a background goroutine reaps the process and logs its exit.
type LocalTrigger struct {
	binary string
	logger *slog.Logger
}

// NewLocalTrigger returns a LocalTrigger that spawns the runner at binaryPath.
func NewLocalTrigger(binaryPath string, logger *slog.Logger) *LocalTrigger {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &LocalTrigger{binary: binaryPath, logger: logger}
}

// Trigger starts the runner with env layered over the parent environment (so the
// child inherits PATH, DOCKER_HOST, etc. that act needs). It returns the Start
// error only; a non-zero runner exit is reported to the report API by the runner
// itself, and reaped here for logging.
func (t *LocalTrigger) Trigger(_ context.Context, env map[string]string) error {
	// Deliberately not tied to the dispatch ctx: the runner outlives the push
	// request that triggered it. It reports its own outcome and bounds its own
	// work.
	cmd := exec.Command(t.binary)
	cmd.Env = append(os.Environ(), flattenEnv(env)...)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start local runner %s: %w", t.binary, err)
	}
	jobID := env["GITMOTE_CI_JOB_ID"]
	go func() {
		if err := cmd.Wait(); err != nil {
			t.logger.Warn("local runner exited nonzero", "job", jobID, "pid", cmd.Process.Pid, "error", err)
		} else {
			t.logger.Debug("local runner finished", "job", jobID, "pid", cmd.Process.Pid)
		}
	}()
	return nil
}

// flattenEnv turns a map into the KEY=VALUE slice exec expects.
func flattenEnv(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}
