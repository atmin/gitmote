package ci

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
)

// NoopTrigger records a run but executes nothing. It is the fallback when no
// runner substrate is configured, neither local nor Scaleway. Dispatch stays
// fire-and-forget, and the run never leaves the queued state.
type NoopTrigger struct{}

// Trigger implements Trigger.
func (NoopTrigger) Trigger(context.Context, map[string]string) error { return nil }

// LocalTrigger runs a CI job by spawning the gitmote-runner binary as a local
// process. This mirrors how the Scaleway client starts a container. The runner
// code and the env contract are the same, but the substrate is local instead
// of a cloud job. LocalTrigger is fire-and-forget. Start (not Wait) returns
// immediately, so dispatch never blocks the push. A background goroutine reaps
// the process and logs its exit.
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

// Trigger starts the runner with env layered over the parent environment. So
// the child inherits the variables that act needs, such as PATH and
// DOCKER_HOST. Trigger returns only the Start error. Trigger reaps the exit
// for logging.
//
// The runner reports a failed job to the report API itself, and then exits 0.
// A non-zero exit is an internal runner failure that the runner could not
// report. Trigger only logs that exit.
func (t *LocalTrigger) Trigger(_ context.Context, env map[string]string) error {
	// The command is deliberately not tied to the dispatch ctx, because the
	// runner outlives the push request that triggered it. The runner reports
	// its own outcome and bounds its own work.
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

// flattenEnv turns a map into the KEY=VALUE slice that exec expects.
func flattenEnv(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}
